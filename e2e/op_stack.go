package e2e

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math"
	"time"

	"github.com/ethereum-optimism/optimism/op-batcher/batcher"
	"github.com/ethereum-optimism/optimism/op-batcher/compressor"
	opbatchermetrics "github.com/ethereum-optimism/optimism/op-batcher/metrics"
	opnodemetrics "github.com/ethereum-optimism/optimism/op-node/metrics"
	opnode "github.com/ethereum-optimism/optimism/op-node/node"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/driver"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sync"
	opproposermetrics "github.com/ethereum-optimism/optimism/op-proposer/metrics"
	"github.com/ethereum-optimism/optimism/op-proposer/proposer"
	opcrypto "github.com/ethereum-optimism/optimism/op-service/crypto"
	"github.com/ethereum-optimism/optimism/op-service/dial"
	"github.com/ethereum-optimism/optimism/op-service/sources"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/polymerdao/monomer/e2e/url"
	"github.com/polymerdao/monomer/utils"
	"github.com/sourcegraph/conc"
)

type OPEventListener interface {
	// LogWithPrefix is meant to be used as a general-purpose log function.
	// The prefix will never be empty and r will never be nil.
	// It is only used to fulfill log interfaces defined by third-party OP Stack components.
	LogWithPrefix(prefix string, r *log.Record)
	AfterStartup()
	BeforeShutdown()
}

type OPStack struct {
	l1URL               *url.URL
	engineURL           *url.URL
	nodeURL             *url.URL
	privKey             *ecdsa.PrivateKey
	rollupConfig        *rollup.Config
	l2OutputOracleProxy common.Address
	eventListener       OPEventListener
}

// TODO setup verifiers

func NewOPStack(
	l1URL,
	engineURL,
	nodeURL *url.URL,
	l2OutputOracleProxy common.Address,
	privKey *ecdsa.PrivateKey,
	rollupConfig *rollup.Config,
	eventListener OPEventListener,
) *OPStack {
	return &OPStack{
		l1URL:               l1URL,
		engineURL:           engineURL,
		nodeURL:             nodeURL,
		privKey:             privKey,
		rollupConfig:        rollupConfig,
		l2OutputOracleProxy: l2OutputOracleProxy,
		eventListener:       eventListener,
	}
}

func (op *OPStack) Run(parentCtx context.Context) (err error) {
	var wg conc.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer func() {
		op.eventListener.BeforeShutdown()
		cancel(err)
		err = utils.Cause(ctx)
	}()

	anvilRPCClient, err := rpc.DialContext(ctx, op.l1URL.String())
	if err != nil {
		return fmt.Errorf("dial anvil: %v", err)
	}
	anvil := NewAnvilClient(anvilRPCClient)

	// Run op-node.
	wg.Go(func() {
		cancel(op.runNode(ctx))
	})

	// Use the same tx manager config for the op-proposer and op-batcher.
	defaults := txmgr.DefaultBatcherFlagValues
	l1ChainID, err := anvil.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("get l1 chain id: %v", err)
	}
	txManagerConfig := &txmgr.Config{
		Backend:                   anvil,
		ChainID:                   l1ChainID,
		NumConfirmations:          defaults.NumConfirmations,
		NetworkTimeout:            defaults.NetworkTimeout,
		FeeLimitMultiplier:        defaults.FeeLimitMultiplier,
		ResubmissionTimeout:       defaults.ResubmissionTimeout,
		ReceiptQueryInterval:      defaults.ReceiptQueryInterval,
		TxNotInMempoolTimeout:     defaults.TxNotInMempoolTimeout,
		SafeAbortNonceTooLowCount: defaults.SafeAbortNonceTooLowCount,
		Signer: func(ctx context.Context, address common.Address, tx *ethtypes.Transaction) (*ethtypes.Transaction, error) {
			return opcrypto.PrivateKeySignerFn(op.privKey, l1ChainID)(address, tx)
		},
	}

	wg.Go(func() {
		cancel(op.runProposer(ctx, anvil, txManagerConfig))
	})

	wg.Go(func() {
		cancel(op.runBatcher(ctx, anvil, txManagerConfig))
	})

	op.eventListener.AfterStartup()
	<-ctx.Done()
	return nil
}

func (op *OPStack) runNode(ctx context.Context) (err error) {
	opNode, err := opnode.New(ctx, &opnode.Config{
		L1: &opnode.L1EndpointConfig{
			L1NodeAddr:     op.l1URL.String(),
			BatchSize:      10,
			MaxConcurrency: 10,
			L1RPCKind:      sources.RPCKindBasic,
		},
		L2: &opnode.L2EndpointConfig{
			L2EngineAddr:      op.engineURL.String(),
			L2EngineJWTSecret: [32]byte{},
		},
		Driver: driver.Config{
			SequencerEnabled: true,
		},
		Rollup: *op.rollupConfig,
		RPC: opnode.RPCConfig{
			ListenAddr: op.nodeURL.Hostname(),
			ListenPort: int(op.nodeURL.PortU16()),
		},
		ConfigPersistence: opnode.DisabledConfigPersistence{},
		Sync: sync.Config{
			SyncMode: sync.CLSync,
		},
	}, op.newLogger("node"), op.newLogger("node snapshotter"), "v0.1", opnodemetrics.NewMetrics(""))
	if err != nil {
		return fmt.Errorf("new node: %v", err)
	}
	if err := opNode.Start(ctx); err != nil {
		return fmt.Errorf("start node: %v", err)
	}
	defer func() {
		err = utils.RunAndWrapOnError(err, "stop node", func() error {
			return opNode.Stop(context.Background())
		})
	}()
	<-ctx.Done()
	return nil
}

func (op *OPStack) runProposer(ctx context.Context, l1Client proposer.L1Client, txManagerConfig *txmgr.Config) (err error) {
	metrics := opproposermetrics.NoopMetrics
	txManager, err := txmgr.NewSimpleTxManagerFromConfig("proposer", op.newLogger("proposer tx manager"), metrics, *txManagerConfig)
	if err != nil {
		return fmt.Errorf("new simple tx manager: %v", err)
	}
	defer txManager.Close()
	rollupProvider, err := dial.NewStaticL2RollupProvider(ctx, op.newLogger("proposer dialer"), op.nodeURL.String())
	if err != nil {
		return fmt.Errorf("new static l2 rollup provider: %v", err)
	}
	defer rollupProvider.Close()
	outputSubmitter, err := proposer.NewL2OutputSubmitter(proposer.DriverSetup{
		Log:  op.newLogger("proposer"),
		Metr: metrics,
		Cfg: proposer.ProposerConfig{
			PollInterval:       50 * time.Millisecond,
			NetworkTimeout:     2 * time.Second,
			L2OutputOracleAddr: utils.Ptr(op.l2OutputOracleProxy),
		},
		Txmgr:          txManager,
		L1Client:       l1Client,
		RollupProvider: rollupProvider,
	})
	if err != nil {
		return fmt.Errorf("new l2 output submitter: %v", err)
	}
	if err := outputSubmitter.StartL2OutputSubmitting(); err != nil {
		return fmt.Errorf("start l2 output submitting: %v", err)
	}
	defer func() {
		err = utils.RunAndWrapOnError(err, "stop l2 output submitting", outputSubmitter.StopL2OutputSubmitting)
	}()
	<-ctx.Done()
	return nil
}

func (op *OPStack) runBatcher(ctx context.Context, l1Client batcher.L1Client, txManagerConfig *txmgr.Config) error {
	metrics := opbatchermetrics.NoopMetrics
	txManager, err := txmgr.NewSimpleTxManagerFromConfig("batcher", op.newLogger("batcher tx manager"), metrics, *txManagerConfig)
	if err != nil {
		return fmt.Errorf("new simple tx manager: %v", err)
	}
	defer txManager.Close()
	endpointProvider, err := dial.NewStaticL2EndpointProvider(
		ctx,
		op.newLogger("batcher dialer"),
		op.engineURL.String(),
		op.nodeURL.String(),
	)
	if err != nil {
		return fmt.Errorf("new static l2 endpoint provider: %v", err)
	}
	batchSubmitter := batcher.NewBatchSubmitter(batcher.DriverSetup{
		Log:          op.newLogger("batcher"),
		Metr:         metrics,
		RollupConfig: op.rollupConfig,
		Config: batcher.BatcherConfig{
			NetworkTimeout: 10 * time.Second,
			PollInterval:   50 * time.Millisecond,
		},
		Txmgr:            txManager,
		L1Client:         l1Client,
		EndpointProvider: endpointProvider,
		ChannelConfig: batcher.ChannelConfig{
			SeqWindowSize:  op.rollupConfig.SeqWindowSize,
			ChannelTimeout: op.rollupConfig.ChannelTimeout,
			// These values are taken from the op-e2e test configs.
			MaxChannelDuration: 1,
			SubSafetyMargin:    4,
			MaxFrameSize:       math.MaxUint64,
			CompressorConfig: compressor.Config{
				TargetFrameSize:  100_000,
				TargetNumFrames:  1,
				ApproxComprRatio: 0.4,
			},
		},
	})
	if err := batchSubmitter.StartBatchSubmitting(); err != nil {
		return fmt.Errorf("start batch submitting: %v", err)
	}
	/*
		There appears to be a deadlock in StopBatchSubmitting.
		This was most likely fixed in a more recent OP-stack version, based on the significant diff.
		defer func() {
			err = utils.RunAndWrapOnError(err, "stop batch submitting", func() error {
				return batchSubmitter.StopBatchSubmitting(ctx)
			})
		}()
	*/
	<-ctx.Done()
	return nil
}

func (op *OPStack) newLogger(prefix string) log.Logger {
	logger := log.New()
	logger.SetHandler(log.FuncHandler(func(r *log.Record) error {
		op.eventListener.LogWithPrefix(prefix, r)
		return nil
	}))
	return logger
}