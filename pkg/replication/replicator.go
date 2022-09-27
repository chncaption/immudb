/*
Copyright 2022 Codenotary Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package replication

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/codenotary/immudb/pkg/api/schema"
	"github.com/codenotary/immudb/pkg/client"
	"github.com/codenotary/immudb/pkg/database"
	"github.com/codenotary/immudb/pkg/logger"
	"github.com/codenotary/immudb/pkg/stream"
	"github.com/rs/xid"
	"google.golang.org/grpc/metadata"
)

var ErrIllegalArguments = errors.New("illegal arguments")
var ErrAlreadyRunning = errors.New("already running")
var ErrAlreadyStopped = errors.New("already stopped")
var ErrFollowerDivergedFromMaster = errors.New("follower diverged from master")

type TxReplicator struct {
	uuid xid.ID

	db   database.DB
	opts *Options

	masterDB string

	logger logger.Logger

	mainContext context.Context
	cancelFunc  context.CancelFunc

	streamSrvFactory stream.ServiceFactory
	client           client.ImmuClient
	clientContext    context.Context

	lastTx uint64

	prefetchTxBuffer       chan []byte // buffered channel of exported txs
	replicationConcurrency int

	allowTxDiscarding bool

	delayer             Delayer
	consecutiveFailures int

	running bool

	mutex sync.Mutex
}

func NewTxReplicator(uuid xid.ID, db database.DB, opts *Options, logger logger.Logger) (*TxReplicator, error) {
	if db == nil || logger == nil || opts == nil || !opts.Valid() {
		return nil, ErrIllegalArguments
	}

	return &TxReplicator{
		uuid:                   uuid,
		db:                     db,
		opts:                   opts,
		logger:                 logger,
		masterDB:               fullAddress(opts.masterDatabase, opts.masterAddress, opts.masterPort),
		streamSrvFactory:       stream.NewStreamServiceFactory(opts.streamChunkSize),
		prefetchTxBuffer:       make(chan []byte, opts.prefetchTxBufferSize),
		replicationConcurrency: opts.replicationCommitConcurrency,
		allowTxDiscarding:      opts.allowTxDiscarding,
		delayer:                opts.delayer,
	}, nil
}

func (txr *TxReplicator) handleError(err error) (terminate bool) {
	txr.mutex.Lock()
	defer txr.mutex.Unlock()

	if err == nil {
		txr.consecutiveFailures = 0
		return false
	}

	txr.consecutiveFailures++

	txr.logger.Infof("Replication error on database '%s' from '%s' (%d consecutive failures). Reason: %v",
		txr.db.GetName(),
		txr.masterDB,
		txr.consecutiveFailures,
		err)

	timer := time.NewTimer(txr.delayer.DelayAfter(txr.consecutiveFailures))
	select {
	case <-txr.mainContext.Done():
		timer.Stop()
		return true
	case <-timer.C:
	}

	if txr.consecutiveFailures >= 3 {
		txr.disconnect()
	}

	return false
}

func (txr *TxReplicator) Start() error {
	txr.mutex.Lock()
	defer txr.mutex.Unlock()

	if txr.running {
		return ErrAlreadyRunning
	}

	txr.logger.Infof("Initializing replication from '%s' to '%s'...", txr.masterDB, txr.db.GetName())

	txr.mainContext, txr.cancelFunc = context.WithCancel(context.Background())

	txr.running = true

	go func() {
		for {
			err := txr.fetchNextTx()
			if err == ErrAlreadyStopped {
				return
			}
			if err == ErrFollowerDivergedFromMaster {
				txr.Stop()
				return
			}

			if txr.handleError(err) {
				return
			}
		}
	}()

	for i := 0; i < txr.replicationConcurrency; i++ {
		go func() {
			for etx := range txr.prefetchTxBuffer {
				consecutiveFailures := 0

				// replication must be retried as many times as necessary
				for {
					_, err := txr.db.ReplicateTx(etx)
					if err == nil {
						break // transaction successfully replicated
					}
					if err == ErrAlreadyStopped {
						return
					}

					if strings.Contains(err.Error(), "tx already committed") {
						break // transaction successfully replicated
					}

					txr.logger.Infof("Failed to replicate transaction from '%s' to '%s'. Reason: %v", txr.masterDB, txr.db.GetName(), err)

					consecutiveFailures++

					timer := time.NewTimer(txr.delayer.DelayAfter(consecutiveFailures))
					select {
					case <-txr.mainContext.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}

	txr.logger.Infof("Replication from '%s' to '%s' succesfully initialized", txr.masterDB, txr.db.GetName())

	return nil
}

func fullAddress(db, address string, port int) string {
	return fmt.Sprintf("%s@%s:%d", db, address, port)
}

func (txr *TxReplicator) connect() error {
	txr.logger.Infof("Connecting to '%s':'%d' for database '%s'...",
		txr.opts.masterAddress,
		txr.opts.masterPort,
		txr.db.GetName())

	opts := client.DefaultOptions().WithAddress(txr.opts.masterAddress).WithPort(txr.opts.masterPort)
	client, err := client.NewImmuClient(opts)
	if err != nil {
		return err
	}

	login, err := client.Login(txr.mainContext, []byte(txr.opts.followerUsername), []byte(txr.opts.followerPassword))
	if err != nil {
		return err
	}

	txr.clientContext = metadata.NewOutgoingContext(txr.mainContext, metadata.Pairs("authorization", login.GetToken()))

	udr, err := client.UseDatabase(txr.clientContext, &schema.Database{DatabaseName: txr.opts.masterDatabase})
	if err != nil {
		return err
	}

	txr.clientContext = metadata.NewOutgoingContext(txr.clientContext, metadata.Pairs("authorization", udr.GetToken()))

	txr.client = client

	txr.logger.Infof("Connection to '%s':'%d' for database '%s' successfully established",
		txr.opts.masterAddress,
		txr.opts.masterPort,
		txr.db.GetName())

	return nil
}

func (txr *TxReplicator) disconnect() {
	if txr.client == nil {
		return
	}

	txr.logger.Infof("Disconnecting from '%s':'%d' for database '%s'...", txr.opts.masterAddress, txr.opts.masterPort, txr.db.GetName())

	txr.client.Logout(txr.clientContext)
	txr.client.Disconnect()

	txr.client = nil

	txr.logger.Infof("Disconnected from '%s':'%d' for database '%s'", txr.opts.masterAddress, txr.opts.masterPort, txr.db.GetName())
}

func (txr *TxReplicator) fetchNextTx() error {
	txr.mutex.Lock()
	defer txr.mutex.Unlock()

	if !txr.running {
		return ErrAlreadyStopped
	}

	if txr.client == nil {
		err := txr.connect()
		if err != nil {
			return err
		}
	}

	commitState, err := txr.db.CurrentState()
	if err != nil {
		return err
	}

	syncReplicationEnabled := txr.db.IsSyncReplicationEnabled()

	if txr.lastTx == 0 {
		txr.lastTx = commitState.PrecommittedTxId
	}

	nextTx := txr.lastTx + 1

	var state *schema.FollowerState

	if syncReplicationEnabled {
		state = &schema.FollowerState{
			UUID:             txr.uuid.String(),
			CommittedTxID:    commitState.TxId,
			CommittedAlh:     commitState.TxHash,
			PrecommittedTxID: commitState.PrecommittedTxId,
			PrecommittedAlh:  commitState.PrecommittedTxHash,
		}
	}

	exportTxStream, err := txr.client.ExportTx(txr.clientContext, &schema.ExportTxRequest{
		Tx:                nextTx,
		FollowerState:     state,
		AllowPreCommitted: syncReplicationEnabled,
	})
	if err != nil {
		if strings.Contains(err.Error(), "follower commit state diverged from master's") {
			txr.logger.Errorf("follower commit state at '%s' diverged from master's", txr.db.GetName())
			return ErrFollowerDivergedFromMaster
		}

		if strings.Contains(err.Error(), "follower precommit state diverged from master's") {

			if !txr.allowTxDiscarding {
				txr.logger.Errorf("follower precommit state at '%s' diverged from master's", txr.db.GetName())
				return ErrFollowerDivergedFromMaster
			}

			txr.logger.Infof("discarding precommit txs since %d from '%s'...", nextTx, txr.db.GetName(), err)

			err = txr.db.DiscardPrecommittedTxsSince(commitState.TxId + 1)
			if err != nil {
				return err
			}

			txr.logger.Infof("precommit txs successfully discarded from '%s'", txr.db.GetName())

		}

		return err
	}

	receiver := txr.streamSrvFactory.NewMsgReceiver(exportTxStream)
	etx, err := receiver.ReadFully()

	if err != nil && err != io.EOF {
		return err
	}

	if syncReplicationEnabled {
		md := exportTxStream.Trailer()

		if len(md.Get("may-commit-up-to-txid-bin")) == 0 || len(md.Get("may-commit-up-to-alh-bin")) == 0 {
			return fmt.Errorf("master is not running with synchronous replication")
		}

		mayCommitUpToTxID := binary.BigEndian.Uint64([]byte(md.Get("may-commit-up-to-txid-bin")[0]))

		var mayCommitUpToAlh [sha256.Size]byte
		copy(mayCommitUpToAlh[:], []byte(md.Get("may-commit-up-to-alh-bin")[0]))

		if mayCommitUpToTxID > 0 {
			err = txr.db.AllowCommitUpto(mayCommitUpToTxID, mayCommitUpToAlh)
			if err != nil {
				if strings.Contains(err.Error(), "follower commit state diverged from master's") {
					txr.logger.Errorf("follower commit state at '%s' diverged from master's", txr.db.GetName())
					return ErrFollowerDivergedFromMaster
				}

				return err
			}
		}
	}

	if len(etx) > 0 {
		// in some cases the transaction is not provided but only the master commit state
		txr.prefetchTxBuffer <- etx
		txr.lastTx++
	}

	return nil
}

func (txr *TxReplicator) Stop() error {
	if txr.cancelFunc != nil {
		txr.cancelFunc()
	}

	txr.mutex.Lock()
	defer txr.mutex.Unlock()

	txr.logger.Infof("Stopping replication of database '%s'...", txr.db.GetName())

	if !txr.running {
		return ErrAlreadyStopped
	}

	close(txr.prefetchTxBuffer)

	txr.disconnect()

	txr.running = false

	txr.logger.Infof("Replication of database '%s' successfully stopped", txr.db.GetName())

	return nil
}
