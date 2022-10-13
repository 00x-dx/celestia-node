package ipld

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	mh "github.com/multiformats/go-multihash"
	"go.opentelemetry.io/otel"

	"github.com/celestiaorg/celestia-app/pkg/appconsts"
	"github.com/celestiaorg/celestia-app/pkg/da"
	"github.com/celestiaorg/nmt"
)

var (
	tracer = otel.Tracer("ipld")
	log    = logging.Logger("ipld")
)

const (
	// Below used multiformats (one codec, one multihash) seem free:
	// https://github.com/multiformats/multicodec/blob/master/table.csv

	// nmtCodec is the codec used for leaf and inner nodes of a Namespaced Merkle Tree.
	nmtCodec = 0x7700

	// sha256Namespace8Flagged is the multihash code used to hash blocks
	// that contain an NMT node (inner and leaf nodes).
	sha256Namespace8Flagged = 0x7701

	// nmtHashSize is the size of a digest created by an NMT in bytes.
	nmtHashSize = 2*appconsts.NamespaceSize + sha256.Size

	// MaxSquareSize is currently the maximum size supported for unerasured data in rsmt2d.ExtendedDataSquare.
	MaxSquareSize = appconsts.MaxSquareSize

	// NamespaceSize is a system-wide size for NMT namespaces.
	NamespaceSize = appconsts.NamespaceSize

	// cidPrefixSize is the size of the prepended buffer of the CID encoding
	// for NamespacedSha256. For more information, see:
	// https://multiformats.io/multihash/#the-multihash-format
	cidPrefixSize = 4
)

func GetNode(ctx context.Context, bGetter blockservice.BlockGetter, root cid.Cid) (ipld.Node, error) {
	block, err := bGetter.GetBlock(ctx, root)
	if err != nil {
		var errNotFound *ipld.ErrNotFound
		if errors.As(err, &errNotFound) {
			return nil, errNotFound
		}
		return nil, err
	}

	return decodeBlock(block)
}

func decodeBlock(block blocks.Block) (ipld.Node, error) {
	innerNodeSize, leafNodeSize := (nmtHashSize)*2, NamespaceSize+consts.ShareSize
	switch len(block.RawData()) {
	default:
		return nil, fmt.Errorf("ipld: wrong sized data carried in block")
	case innerNodeSize:
		return &nmtNode{block}, nil
	case leafNodeSize:
		return &nmtLeafNode{nmtNode{block}}, nil
	}
}

var _ ipld.Node = (*nmtNode)(nil)
var _ ipld.Node = (*nmtLeafNode)(nil)

type nmtNode struct {
	blocks.Block
}

func newNMTNode(id cid.Cid, data []byte) nmtNode {
	b, err := blocks.NewBlockWithCid(data, id)
	if err != nil {
		panic(fmt.Sprintf("wrong hash for block, cid: %s", id))
	}
	return nmtNode{b}
}

func (n nmtNode) Resolve(path []string) (interface{}, []string, error) {
	switch path[0] {
	case "0":
		left, err := CidFromNamespacedSha256(n.left())
		if err != nil {
			return nil, nil, err
		}
		return &ipld.Link{Cid: left}, path[1:], nil
	case "1":
		right, err := CidFromNamespacedSha256(n.right())
		if err != nil {
			return nil, nil, err
		}
		return &ipld.Link{Cid: right}, path[1:], nil
	default:
		return nil, nil, errors.New("invalid path for inner node")
	}
}

func (n nmtNode) Tree(path string, depth int) []string {
	if path != "" || depth != -1 {
		panic("proper tree not yet implemented")
	}

	return []string{
		"0",
		"1",
	}
}

func (n nmtNode) ResolveLink(path []string) (*ipld.Link, []string, error) {
	obj, rest, err := n.Resolve(path)
	if err != nil {
		return nil, nil, err
	}

	lnk, ok := obj.(*ipld.Link)
	if !ok {
		return nil, nil, errors.New("was not a link")
	}

	return lnk, rest, nil
}

func (n nmtNode) Copy() ipld.Node {
	d := make([]byte, len(n.RawData()))
	copy(d, n.RawData())
	return newNMTNode(n.Cid(), d)
}

func (n nmtNode) Links() []*ipld.Link {
	leftCid := MustCidFromNamespacedSha256(n.left())
	rightCid := MustCidFromNamespacedSha256(n.right())

	return []*ipld.Link{{Cid: leftCid}, {Cid: rightCid}}
}

func (n nmtNode) Stat() (*ipld.NodeStat, error) {
	return &ipld.NodeStat{}, nil
}

func (n nmtNode) Size() (uint64, error) {
	return 0, nil
}

func (n nmtNode) left() []byte {
	return n.RawData()[:nmtHashSize]
}

func (n nmtNode) right() []byte {
	return n.RawData()[nmtHashSize:]
}

type nmtLeafNode struct {
	nmtNode
}

func newNMTLeafNode(id cid.Cid, data []byte) nmtLeafNode {
	return nmtLeafNode{newNMTNode(id, data)}
}

func (l nmtLeafNode) Resolve(path []string) (interface{}, []string, error) {
	return nil, nil, errors.New("invalid path for leaf node")
}

func (l nmtLeafNode) Tree(_path string, _depth int) []string {
	return nil
}

func (l nmtLeafNode) Links() []*ipld.Link {
	return nil
}

// CidFromNamespacedSha256 uses a hash from an nmt tree to create a CID
func CidFromNamespacedSha256(namespacedHash []byte) (cid.Cid, error) {
	if got, want := len(namespacedHash), nmtHashSize; got != want {
		return cid.Cid{}, fmt.Errorf("invalid namespaced hash length, got: %v, want: %v", got, want)
	}
	buf, err := mh.Encode(namespacedHash, sha256Namespace8Flagged)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(nmtCodec, buf), nil
}

// MustCidFromNamespacedSha256 is a wrapper around cidFromNamespacedSha256 that panics
// in case of an error. Use with care and only in places where no error should occur.
func MustCidFromNamespacedSha256(hash []byte) cid.Cid {
	cidFromHash, err := CidFromNamespacedSha256(hash)
	if err != nil {
		panic(
			fmt.Sprintf("malformed hash: %s, codec: %v",
				err,
				mh.Codes[sha256Namespace8Flagged]),
		)
	}
	return cidFromHash
}

// Translate transforms square coordinates into IPLD NMT tree path to a leaf node.
// It also adds randomization to evenly spread fetching from Rows and Columns.
func Translate(dah *da.DataAvailabilityHeader, row, col int) (cid.Cid, int) {
	if rand.Intn(2) == 0 { //nolint:gosec
		return MustCidFromNamespacedSha256(dah.ColumnRoots[col]), row
	}

	return MustCidFromNamespacedSha256(dah.RowsRoots[row]), col
}

// NamespacedSha256FromCID derives the Namespaced hash from the given CID.
func NamespacedSha256FromCID(cid cid.Cid) []byte {
	return cid.Hash()[cidPrefixSize:]
}