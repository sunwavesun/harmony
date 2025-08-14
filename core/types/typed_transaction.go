package types

import (
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	LegacyTxType = iota
	AccessListTxType
)

// Transaction is an Ethereum transaction.
type Transaction struct {
	Type byte
	*LegacyTx
	*AccessListTx
}

// LegacyTx is the transaction data of regular Ethereum transactions.
type LegacyTx struct {
	Nonce     uint64          // nonce of sender account
	GasPrice  *big.Int        // wei per gas
	Gas       uint64          // gas limit
	To        *common.Address `rlp:"nil"` // nil means contract creation
	Value     *big.Int        // wei amount
	Data      []byte          // contract invocation input data
	ShardID   uint32
	ToShardID uint32
	V, R, S   *big.Int // signature values
}

// AccessListTx is the data of EIP-2930 access list transactions.
type AccessListTx struct {
	ChainID    *big.Int // chain ID of the transaction
	Nonce      uint64   // nonce of sender account
	GasPrice   *big.Int // wei per gas
	Gas        uint64   // gas limit
	To         *common.Address
	Value      *big.Int
	Data       []byte
	AccessList AccessList
	ShardID    uint32
	ToShardID  uint32
	V, R, S    *big.Int // signature values
}

// AccessList is an EIP-2930 access list.
type AccessList []AccessTuple

// AccessTuple is the element type of an access list.
type AccessTuple struct {
	Address     common.Address
	StorageKeys []common.Hash
}

type accessListMarshaling []accessTupleMarshaling

type accessTupleMarshaling struct {
	Address     common.Address
	StorageKeys []common.Hash
}

type txMarshaling struct {
	Type hexutil.Uint
	// Common fields for all tx types
	Nonce    hexutil.Uint64
	GasPrice *hexutil.Big
	Gas      hexutil.Uint64
	To       *common.Address
	Value    *hexutil.Big
	Data     hexutil.Bytes
	V, R, S  *hexutil.Big

	// EIP-2930
	AccessList *AccessList
	ChainID    *hexutil.Big
}

// rlp wrappers for tx struct
type rlpLegacyTx struct {
	Nonce     uint64
	GasPrice  *big.Int
	Gas       uint64
	To        *common.Address
	Value     *big.Int
	Data      []byte
	ShardID   uint32
	ToShardID uint32
	V, R, S   *big.Int
}

type rlpAccessListTx struct {
	ChainID    *big.Int
	Nonce      uint64
	GasPrice   *big.Int
	Gas        uint64
	To         *common.Address
	Value      *big.Int
	Data       []byte
	AccessList AccessList
	ShardID    uint32
	ToShardID  uint32
	V, R, S    *big.Int
}

// encodeRLP implements rlp.Encoder
func (tx *LegacyTx) encodeRLP(w io.Writer) error {
	return rlp.Encode(w, (*rlpLegacyTx)(tx))
}

// encodeRLP implements rlp.Encoder
func (tx *AccessListTx) encodeRLP(w io.Writer) error {
	return rlp.Encode(w, (*rlpAccessListTx)(tx))
}

func (tx *Transaction) rlpData() interface{} {
	switch tx.Type {
	case LegacyTxType:
		return (*rlpLegacyTx)(tx.LegacyTx)
	case AccessListTxType:
		return (*rlpAccessListTx)(tx.AccessListTx)
	default:
		panic("invalid tx type") // Should not happen
	}
}

func (tx *Transaction) rlpSignatureData() interface{} {
	switch tx.Type {
	case LegacyTxType:
		return (*rlpLegacyTx)(tx.LegacyTx)
	case AccessListTxType:
		return (*rlpAccessListTx)(tx.AccessListTx)
	default:
		panic("invalid tx type") // Should not happen
	}
}
