/* Mysterium network payment library.
 *
 * Copyright (C) 2021 BlockDev AG
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 * You should have received a copy of the GNU Lesser General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package transfer

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
)

// GasPriceIncremenetor exposes a way automatically increment gas fees
// for transactions in a preconfigured manner.
type GasPriceIncremenetor struct {
	storage Storage
	bc      MultichainClient

	cfg     GasIncrementorConfig
	signers safeSigners

	syncer *syncer
	logFn  LogFunc
	stop   chan struct{}
	once   sync.Once
}

// GasIncrementorConfig is provided to the incrementor to configure it.
type GasIncrementorConfig struct {
	PullInterval      time.Duration
	MaxQueuePerSigner int
}

// Storage is given to the Incremeter to be used to
// insert, update or get transactions.
type Storage interface {
	// UpsertIncrementorTransaction is called to upsert a transaction.
	// It either inserts a new entry or updates existing entries.
	UpsertIncrementorTransaction(tx Transaction) error

	// GetIncrementorTransactionsToCheck returns all transaction that need to rechecked.
	//
	// Entries should be filtered by possible signers. If incrementor cannot sign the transaction
	// it should not received it.
	GetIncrementorTransactionsToCheck(possibleSigners []string) (tx []Transaction, err error)

	// GasIncrementorSenderQueue returns the length of a queue for a single sender.
	GetIncrementorSenderQueue(sender string) (length int, err error)
}

// MultichainClient handles calls to BC.
type MultichainClient interface {
	TransactionReceipt(chainID int64, hash common.Hash) (*types.Receipt, error)
	SendTransaction(chainID int64, tx *types.Transaction) error
	TransactionByHash(chainID int64, hash common.Hash) (*types.Transaction, bool, error)
}

// LogFunc can be attacheched to Incrementer to enable logging.
type LogFunc func(Transaction, error)

// NewGasPriceIncremenetor returns a new incrementer instance.
func NewGasPriceIncremenetor(cfg GasIncrementorConfig, storage Storage, cl MultichainClient, signers Signers) *GasPriceIncremenetor {
	return &GasPriceIncremenetor{
		storage: storage,
		bc:      cl,

		cfg: cfg,
		signers: safeSigners{
			signers: signers,
		},

		syncer: newSyncer(),
		stop:   make(chan struct{}, 0),
	}
}

// Run starts the gas price incrementer.
//
// It will query the given storage for any entries that it needs to check
// for gas increase, trying to check their status.
func (i *GasPriceIncremenetor) Run() {
	process := func(txs []Transaction) {
		for _, tx := range txs {
			switch tx.State {
			case TxStateFailed, TxStateSucceed:
				// Force skip transactions that are finalized.
			default:
				i.tryWatch(tx)
			}
		}
	}

	for {
		select {
		case <-i.stop:
			return

		case <-time.After(i.cfg.PullInterval):
			txs, err := i.storage.GetIncrementorTransactionsToCheck(i.signers.getSigners())
			if err != nil {
				continue
			}
			process(txs)
		}
	}
}

// AttachLogFunc attaches a new log func incremeter thread.
// The given log func is called every time the running incrementer thread
// created by the `Run` method encouters an error.
//
// This method is not thread safe and should be called before Run.
func (i *GasPriceIncremenetor) AttachLogFunc(logFn LogFunc) {
	i.logFn = logFn
}

// Stop stops the execution of GasPriceIncrementer thread created by the Run method.
func (i *GasPriceIncremenetor) Stop() {
	i.once.Do(func() {
		close(i.stop)
	})
}

// InsertInitial uses the given storage to insert an new transaction which
// will later be retreived using `GetTransactionsToCheck` in order to check
// it's state and retry with higher gas price if needed.
func (i *GasPriceIncremenetor) InsertInitial(tx *types.Transaction, opts TransactionOpts, senderAddress common.Address) error {
	if err := opts.validate(); err != nil {
		return fmt.Errorf("invalid opts given: %w", err)
	}
	newTx, err := newTransaction(tx, senderAddress, opts)
	if err != nil {
		return fmt.Errorf("failed to create new transaction: %w", err)
	}

	return i.storage.UpsertIncrementorTransaction(*newTx)
}

// CanSign returns if incrementor is able to sign transactions for the given sender.
func (i *GasPriceIncremenetor) CanSign(sender common.Address) bool {
	_, ok := i.signers.getSignerFunc(sender.Hex())
	return ok
}

// CanQueue returns if incrementor is able to queue a transaction
func (i *GasPriceIncremenetor) CanQueue(sender common.Address) (bool, error) {
	length, err := i.storage.GetIncrementorSenderQueue(sender.Hex())
	if err != nil {
		return false, err
	}

	return length < i.cfg.MaxQueuePerSigner, nil
}

// tryWatch will try to watch a transaction.
// If a transaction is already being watched, it will get skipped.
func (i *GasPriceIncremenetor) tryWatch(tx Transaction) {
	if i.syncer.txBeingWatched(tx) {
		// Already watching
		return
	}
	if err := tx.Opts.validate(); err != nil {
		i.log(tx, fmt.Errorf("can't increment gas price, got wrong tx opts: %w", err))
		return
	}

	i.syncer.txMarkBeingWatched(tx)
	go func() {
		defer i.syncer.txRemoveWatched(tx)
		if err := i.watchAndIncrement(tx); err != nil {
			i.log(tx, err)

			if !tx.isExpired() {
				return
			}

			if err := i.transactionFailed(tx); err != nil {
				i.log(tx, err)
			}
		}

	}()
}

func (i *GasPriceIncremenetor) watchAndIncrement(tx Transaction) error {
	timeout := time.After(tx.Opts.Timeout)
	incTimer := time.NewTicker(tx.Opts.IncreaseInterval)
	defer incTimer.Stop()

	checkTimer := time.NewTicker(tx.Opts.CheckInterval)
	defer checkTimer.Stop()

	for {
		select {
		case <-i.stop:
			return nil
		case <-checkTimer.C:
			status, err := i.getTxStatus(tx)
			if err != nil {
				if !i.isBlockchainErrorUnhandleable(err) {
					return err
				}
				i.log(tx, fmt.Errorf("received unhandleable receipt error, marking tx as failed: %w", err))
				return i.transactionFailed(tx)
			}
			if status == StatusSucceeded {
				return i.transactionSuccess(tx)
			}
		case <-incTimer.C:
			newTx, err := i.increaseGasPrice(tx)
			if err != nil {
				if !i.isBlockchainErrorUnhandleable(err) {
					return err
				}
				i.log(tx, fmt.Errorf("received unhandleable increase error, marking tx as failed: %w", err))
				return i.transactionFailed(tx)
			}
			tx = newTx
		case <-timeout:
			return i.transactionFailed(tx)
		}
	}
}

func (i *GasPriceIncremenetor) isBlockchainErrorUnhandleable(err error) bool {
	if errors.Is(err, core.ErrNonceTooHigh) || errors.Is(err, core.ErrNonceTooLow) || errors.Is(err, ethereum.NotFound) {
		return true
	}

	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		return false
	}

	// ethereum sometimes returns errors from some other, non core package, so resort to string checks
	switch unwrapped.Error() {
	case core.ErrNonceTooLow.Error(), core.ErrNonceTooHigh.Error():
		return true
	default:
		return false
	}
}

func (i *GasPriceIncremenetor) increaseGasPrice(tx Transaction) (Transaction, error) {
	org, err := tx.getLatestTx()
	if err != nil {
		return Transaction{}, err
	}

	newGasPrice, _ := new(big.Float).Mul(
		big.NewFloat(tx.Opts.PriceMultiplier),
		new(big.Float).SetInt(org.GasPrice()),
	).Int(nil)

	if newGasPrice.Cmp(tx.Opts.MaxPrice) > 0 {
		if err := i.transactionFailed(tx); err != nil {
			return Transaction{}, err
		}

		return Transaction{}, fmt.Errorf("transaction with uniqueID '%s' failed, gas price limit of %s reached on chain %d", tx.UniqueID, tx.Opts.MaxPrice.String(), tx.ChainID)
	}

	newTx, err := i.signAndSend(tx.rebuiledWithNewGasPrice(org, newGasPrice), tx.ChainID, tx.SenderAddressHex)
	if err != nil {
		return Transaction{}, i.transactionFailed(tx)
	}

	return i.transactionPriceIncreased(tx, newTx)
}

// BCTxStatus represents the status of tx on blockchain.
type BCTxStatus string

const (
	// StatusPending indicates that the tx is still pending.
	StatusPending BCTxStatus = "Pending"
	// StatusFailed indicates that the tx has failed.
	StatusFailed BCTxStatus = "Failed"
	// StatusSucceeded indicates that the tx has suceeded.
	StatusSucceeded BCTxStatus = "Succeeded"
)

func (i *GasPriceIncremenetor) getTxStatus(tx Transaction) (BCTxStatus, error) {
	org, err := tx.getLatestTx()
	if err != nil {
		return StatusFailed, fmt.Errorf("can't get tx status, malformed internal tx object: %w", err)
	}

	hash := org.Hash()
	_, pending, err := i.bc.TransactionByHash(tx.ChainID, hash)
	if err != nil {
		return StatusFailed, fmt.Errorf("failed to get transaction by hash: %w", err)
	}

	if pending {
		return StatusPending, nil
	}

	receipt, err := i.bc.TransactionReceipt(tx.ChainID, hash)
	if err != nil {
		return StatusFailed, fmt.Errorf("failed to get transaction receipt: %w", err)
	}

	return i.bcTxStatusFromReceipt(tx, receipt), nil
}

func (i *GasPriceIncremenetor) bcTxStatusFromReceipt(tx Transaction, rcp *types.Receipt) BCTxStatus {
	switch rcp.Status {
	case types.ReceiptStatusSuccessful:
		return StatusSucceeded
	case types.ReceiptStatusFailed:
		return StatusFailed
	default:
		hash := ""
		if org, err := tx.getLatestTx(); err == nil {
			hash = org.Hash().Hex()
		}

		i.log(tx, fmt.Errorf("received unknown receipt status %v for tx uniqueID: '%v', lastHash: '%v'", rcp.Status, tx.UniqueID, hash))
		return StatusFailed
	}
}

func (i *GasPriceIncremenetor) signAndSend(tx *types.Transaction, chainID int64, senderAddrHex string) (*types.Transaction, error) {
	signer, ok := i.signers.getSignerFunc(senderAddrHex)
	if !ok {
		return nil, fmt.Errorf("can't retry, no signer for address: %s", senderAddrHex)
	}

	signedTx, err := signer(tx, chainID)
	if err != nil {
		return nil, fmt.Errorf("failed to sign a transaction: %w", err)
	}

	if err := i.bc.SendTransaction(chainID, signedTx); err != nil {
		return nil, fmt.Errorf("failed send a transaction: %w", err)
	}

	return signedTx, nil
}

func (i *GasPriceIncremenetor) transactionFailed(tx Transaction) error {
	tx.State = TxStateFailed
	if err := i.storage.UpsertIncrementorTransaction(tx); err != nil {
		return fmt.Errorf("failed marking transaction as failed: %w", err)
	}

	return nil
}

func (i *GasPriceIncremenetor) transactionSuccess(tx Transaction) error {
	tx.State = TxStateSucceed
	if err := i.storage.UpsertIncrementorTransaction(tx); err != nil {
		return fmt.Errorf("failed marking transaction succeed: %w", err)
	}
	return nil
}

func (i *GasPriceIncremenetor) transactionPriceIncreased(tx Transaction, newTx *types.Transaction) (Transaction, error) {
	var err error
	tx.State = TxStatePriceIncreased
	tx.LatestTx, err = newTx.MarshalJSON()
	if err != nil {
		return Transaction{}, fmt.Errorf("failed to marshal internal transaction object: %w", err)
	}

	if err := i.storage.UpsertIncrementorTransaction(tx); err != nil {
		return Transaction{}, fmt.Errorf("failed to update transaction after price increase: %w", err)
	}

	return tx, nil
}

func (i *GasPriceIncremenetor) log(tx Transaction, err error) {
	if i.logFn != nil {
		i.logFn(tx, err)
	}
}

// syncer is used to sync Incrementor so that
// we dont start tracking the same transaction multiple times.
type syncer struct {
	txs map[string]struct{}
	m   sync.Mutex
}

func newSyncer() *syncer {
	return &syncer{txs: make(map[string]struct{})}
}

func (s *syncer) txMarkBeingWatched(tx Transaction) {
	s.m.Lock()
	defer s.m.Unlock()
	s.txs[tx.UniqueID] = struct{}{}
}

func (s *syncer) txBeingWatched(tx Transaction) bool {
	s.m.Lock()
	defer s.m.Unlock()
	_, ok := s.txs[tx.UniqueID]
	return ok
}

func (s *syncer) txRemoveWatched(tx Transaction) {
	s.m.Lock()
	defer s.m.Unlock()
	delete(s.txs, tx.UniqueID)
}

// SignatureFunc is used to sign transactions when resubmitting them.
type SignatureFunc func(tx *types.Transaction, chainID int64) (*types.Transaction, error)

// Signers is a map that holds all possible signers to sign transactions when resending to the blockchain.
type Signers map[common.Address]SignatureFunc

type safeSigners struct {
	signers map[common.Address]SignatureFunc
	m       sync.Mutex
}

func (s *safeSigners) getSignerFunc(senderAddressHex string) (SignatureFunc, bool) {
	s.m.Lock()
	defer s.m.Unlock()

	// TODO: Remove later. Left for backwards compatability.
	// If only one signer is present and senderAddress is `""` return first signer.
	if len(s.signers) == 1 && (senderAddressHex == "" || senderAddressHex == common.HexToAddress("").Hex()) {
		for _, v := range s.signers {
			return v, true
		}
	}

	addr := common.HexToAddress(senderAddressHex)
	signer, ok := s.signers[addr]
	return signer, ok
}

func (s *safeSigners) getSigners() []string {
	s.m.Lock()
	defer s.m.Unlock()

	signers := make([]string, 0)
	for signer := range s.signers {
		signers = append(signers, signer.Hex())
	}
	return signers
}
