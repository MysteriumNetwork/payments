package promises

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mysteriumnetwork/payments/identity"
)

type ExtraData interface {
	Hash() []byte
}

const emptyExtra = "emptyextra"

type EmptyExtra struct {
}

func (EmptyExtra) Hash() []byte {
	return crypto.Keccak256([]byte(emptyExtra))
}

var _ ExtraData = EmptyExtra{}

type Promise struct {
	Extra    ExtraData
	Receiver common.Address
	SeqNo    uint64
	Amount   uint64
}

const issuerPrefix = "Issuer prefix:"

func (p *Promise) Bytes() []byte {
	slices := [][]byte{
		p.Extra.Hash(),
		p.Receiver.Bytes(),
		abi.U256(big.NewInt(0).SetUint64(p.SeqNo)),
		abi.U256(big.NewInt(0).SetUint64(p.Amount)),
	}
	var res []byte
	for _, slice := range slices {
		res = append(res, slice...)
	}
	return res
}

type IssuedPromise struct {
	Promise
	IssuerSignature []byte
}

func (ip *IssuedPromise) Bytes() []byte {
	return append([]byte(issuerPrefix), ip.Promise.Bytes()...)
}

func (ip *IssuedPromise) IssuerAddress() (common.Address, error) {
	publicKey, err := crypto.Ecrecover(crypto.Keccak256(ip.Bytes()), ip.IssuerSignature)
	if err != nil {
		return common.Address{}, err
	}
	pubKey, err := crypto.UnmarshalPubkey(publicKey)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pubKey), nil
}

type ReceivedPromise struct {
	IssuedPromise
	ReceiverSignature []byte
}

func SignByPayer(promise *Promise, payer identity.Signer) (*IssuedPromise, error) {
	signature, err := payer.Sign([]byte(issuerPrefix), promise.Bytes())
	if err != nil {
		return nil, err
	}

	return &IssuedPromise{
		*promise,
		signature,
	}, nil
}

const receiverPrefix = "Receiver prefix:"

func SignByReceiver(promise *IssuedPromise, receiver identity.Signer) (*ReceivedPromise, error) {
	payerAddr, err := promise.IssuerAddress()
	if err != nil {
		return nil, err
	}
	sig, err := receiver.Sign([]byte(receiverPrefix), crypto.Keccak256(promise.Bytes()), payerAddr.Bytes())
	return &ReceivedPromise{
		*promise,
		sig,
	}, nil
}
