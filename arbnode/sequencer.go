// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbnode

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tenderly/nitro/util/headerreader"

	"github.com/tenderly/nitro/go-ethereum/common"
	"github.com/tenderly/nitro/go-ethereum/core"
	"github.com/tenderly/nitro/go-ethereum/core/types"
	"github.com/tenderly/nitro/go-ethereum/core/vm"
	"github.com/tenderly/nitro/go-ethereum/log"
	"github.com/tenderly/nitro/arbos"
	"github.com/tenderly/nitro/arbos/arbosState"
	"github.com/tenderly/nitro/arbos/l1pricing"
	"github.com/tenderly/nitro/util/stopwaiter"
	"github.com/pkg/errors"
)

// 95% of the SequencerInbox limit, leaving ~5KB for headers and such
const maxTxDataSize uint64 = 112065

type txQueueItem struct {
	tx         *types.Transaction
	resultChan chan<- error
	ctx        context.Context
}

func (i *txQueueItem) returnResult(err error) {
	i.resultChan <- err
	close(i.resultChan)
}

type Sequencer struct {
	stopwaiter.StopWaiter

	txStreamer      *TransactionStreamer
	txQueue         chan txQueueItem
	l1Reader        *headerreader.HeaderReader
	config          SequencerConfig
	senderWhitelist map[common.Address]struct{}

	L1BlockAndTimeMutex sync.Mutex
	l1BlockNumber       uint64
	l1Timestamp         uint64

	forwarderMutex sync.Mutex
	forwarder      *TxForwarder
}

func NewSequencer(txStreamer *TransactionStreamer, l1Reader *headerreader.HeaderReader, config SequencerConfig) (*Sequencer, error) {
	senderWhitelist := make(map[common.Address]struct{})
	entries := strings.Split(config.SenderWhitelist, ",")
	for _, address := range entries {
		if len(address) == 0 {
			continue
		}
		if !common.IsHexAddress(address) {
			return nil, fmt.Errorf("sequencer sender whitelist entry \"%v\" is not a valid address", address)
		}
		senderWhitelist[common.HexToAddress(address)] = struct{}{}
	}
	return &Sequencer{
		txStreamer:      txStreamer,
		txQueue:         make(chan txQueueItem, 128),
		l1Reader:        l1Reader,
		config:          config,
		senderWhitelist: senderWhitelist,
		l1BlockNumber:   0,
		l1Timestamp:     0,
	}, nil
}

var ErrRetrySequencer = errors.New("please retry transaction")

func (s *Sequencer) PublishTransaction(ctx context.Context, tx *types.Transaction) error {
	if len(s.senderWhitelist) > 0 {
		signer := types.LatestSigner(s.txStreamer.bc.Config())
		sender, err := types.Sender(signer, tx)
		if err != nil {
			return err
		}
		_, authorized := s.senderWhitelist[sender]
		if !authorized {
			return errors.New("transaction sender is not on the whitelist")
		}
	}

	resultChan := make(chan error, 1)
	queueItem := txQueueItem{
		tx,
		resultChan,
		ctx,
	}
	select {
	case s.txQueue <- queueItem:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case res := <-resultChan:
		return res
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Sequencer) preTxFilter(state *arbosState.ArbosState, tx *types.Transaction, sender common.Address) error {
	return nil
}

func (s *Sequencer) postTxFilter(state *arbosState.ArbosState, tx *types.Transaction, sender common.Address, dataGas uint64, receipt *types.Receipt) error {
	if receipt.Status == types.ReceiptStatusFailed && receipt.GasUsed > dataGas && receipt.GasUsed-dataGas <= s.config.MaxRevertGasReject {
		return vm.ErrExecutionReverted
	}
	return nil
}

func (s *Sequencer) ForwardTarget() string {
	s.forwarderMutex.Lock()
	defer s.forwarderMutex.Unlock()
	if s.forwarder == nil {
		return ""
	}
	return s.forwarder.target
}

func (s *Sequencer) ForwardTo(url string) error {
	s.forwarderMutex.Lock()
	defer s.forwarderMutex.Unlock()
	s.forwarder = NewForwarder(url)
	err := s.forwarder.Initialize(s.GetContext())
	if err != nil {
		log.Error("failed to set forward agent", "err", err)
		s.forwarder = nil
	}
	return err
}

func (s *Sequencer) DontForward() {
	s.forwarderMutex.Lock()
	defer s.forwarderMutex.Unlock()
	s.forwarder = nil
}

func (s *Sequencer) forwardIfSet(queueItems []txQueueItem) bool {
	s.forwarderMutex.Lock()
	defer s.forwarderMutex.Unlock()
	if s.forwarder == nil {
		return false
	}
	for _, item := range queueItems {
		item.resultChan <- s.forwarder.PublishTransaction(item.ctx, item.tx)
	}
	return true
}

func (s *Sequencer) sequenceTransactions(ctx context.Context) {
	var txes types.Transactions
	var queueItems []txQueueItem
	var totalBatchSize int
	for {
		var queueItem txQueueItem
		if len(txes) == 0 {
			select {
			case queueItem = <-s.txQueue:
			case <-ctx.Done():
				return
			}
		} else {
			done := false
			select {
			case queueItem = <-s.txQueue:
			default:
				done = true
			}
			if done {
				break
			}
		}
		err := queueItem.ctx.Err()
		if err != nil {
			queueItem.returnResult(err)
			continue
		}
		txBytes, err := queueItem.tx.MarshalBinary()
		if err != nil {
			queueItem.returnResult(err)
			continue
		}
		if len(txBytes) > int(maxTxDataSize) {
			// This tx is too large
			queueItem.returnResult(core.ErrOversizedData)
			continue
		}
		if totalBatchSize+len(txBytes) > int(maxTxDataSize) {
			// This tx would be too large to add to this batch.
			// Attempt to put it back in the queue, but error if the queue is full.
			// Then, end the batch here.
			select {
			case s.txQueue <- queueItem:
			default:
				queueItem.returnResult(core.ErrOversizedData)
			}
			break
		}
		totalBatchSize += len(txBytes)
		txes = append(txes, queueItem.tx)
		queueItems = append(queueItems, queueItem)
	}

	if s.forwardIfSet(queueItems) {
		return
	}

	timestamp := time.Now().Unix()
	s.L1BlockAndTimeMutex.Lock()
	l1Block := s.l1BlockNumber
	l1Timestamp := s.l1Timestamp
	s.L1BlockAndTimeMutex.Unlock()

	if s.l1Reader != nil && (l1Block == 0 || math.Abs(float64(l1Timestamp)-float64(timestamp)) > s.config.MaxAcceptableTimestampDelta.Seconds()) {
		log.Error(
			"cannot sequence: unknown L1 block or L1 timestamp too far from local clock time",
			"l1Block", l1Block,
			"l1Timestamp", l1Timestamp,
			"localTimestamp", timestamp,
		)
		return
	}

	header := &arbos.L1IncomingMessageHeader{
		Kind:        arbos.L1MessageType_L2Message,
		Poster:      l1pricing.BatchPosterAddress,
		BlockNumber: l1Block,
		Timestamp:   uint64(timestamp),
		RequestId:   nil,
		L1BaseFee:   nil,
	}

	hooks := &arbos.SequencingHooks{
		PreTxFilter:            s.preTxFilter,
		PostTxFilter:           s.postTxFilter,
		DiscardInvalidTxsEarly: true,
		TxErrors:               []error{},
	}
	err := s.txStreamer.SequenceTransactions(header, txes, hooks)
	if err == nil && len(hooks.TxErrors) != len(txes) {
		err = fmt.Errorf("unexpected number of error results: %v vs number of txes %v", len(hooks.TxErrors), len(txes))
	}
	if errors.Is(err, ErrRetrySequencer) {
		// we changed roles
		// forward if we have where to
		if s.forwardIfSet(queueItems) {
			return
		}
		// try to add back to queue otherwise
		for _, item := range queueItems {
			select {
			case s.txQueue <- item:
			default:
				item.resultChan <- errors.New("queue full")
			}
		}
		return
	}
	if err != nil {
		log.Warn("error sequencing transactions", "err", err)
		for _, queueItem := range queueItems {
			queueItem.returnResult(err)
		}
		return
	}

	for i, err := range hooks.TxErrors {
		queueItem := queueItems[i]
		if errors.Is(err, core.ErrGasLimit) {
			// There's not enough gas left in the block for this tx.
			// Attempt to re-queue the transaction.
			// If the queue is full, fall through to returning an error.
			select {
			case s.txQueue <- queueItem:
				continue
			default:
			}
		}
		queueItem.returnResult(err)
	}
}

func (s *Sequencer) updateLatestL1Block(header *types.Header) {
	s.L1BlockAndTimeMutex.Lock()
	defer s.L1BlockAndTimeMutex.Unlock()
	if s.l1BlockNumber < header.Number.Uint64() {
		s.l1BlockNumber = header.Number.Uint64()
		s.l1Timestamp = header.Time
	}
}

func (s *Sequencer) Initialize(ctx context.Context) error {
	if s.l1Reader == nil {
		return nil
	}

	header, err := s.l1Reader.LastHeader(ctx)
	if err != nil {
		return err
	}
	s.updateLatestL1Block(header)
	return nil
}

func (s *Sequencer) Start(ctxIn context.Context) error {
	s.StopWaiter.Start(ctxIn)
	if s.l1Reader != nil {
		initialBlockNr := atomic.LoadUint64(&s.l1BlockNumber)
		if initialBlockNr == 0 {
			return errors.New("sequencer not initialized")
		}

		headerChan, cancel := s.l1Reader.Subscribe(false)

		s.LaunchThread(func(ctx context.Context) {
			defer cancel()
			for {
				select {
				case header, ok := <-headerChan:
					if !ok {
						return
					}
					s.updateLatestL1Block(header)
				case <-ctx.Done():
					return
				}
			}
		})

	}

	s.CallIteratively(func(ctx context.Context) time.Duration {
		nextBlock := time.Now().Add(s.config.MaxBlockSpeed)
		s.sequenceTransactions(ctx)
		// Note: this may return a negative duration, but timers are fine with that (they treat negative durations as 0).
		return time.Until(nextBlock)
	})

	return nil
}
