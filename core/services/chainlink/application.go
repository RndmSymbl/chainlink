package chainlink

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"reflect"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"go.uber.org/multierr"
	"go.uber.org/zap/zapcore"

	pkgsolana "github.com/smartcontractkit/chainlink-solana/pkg/solana"
	pkgterra "github.com/smartcontractkit/chainlink-terra/pkg/terra"
	"github.com/smartcontractkit/sqlx"

	"github.com/smartcontractkit/chainlink/core/bridges"
	"github.com/smartcontractkit/chainlink/core/chains/evm"
	"github.com/smartcontractkit/chainlink/core/chains/evm/txmgr"
	evmtypes "github.com/smartcontractkit/chainlink/core/chains/evm/types"
	"github.com/smartcontractkit/chainlink/core/chains/solana"
	"github.com/smartcontractkit/chainlink/core/chains/terra"
	"github.com/smartcontractkit/chainlink/core/config"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services"
	"github.com/smartcontractkit/chainlink/core/services/blockhashstore"
	"github.com/smartcontractkit/chainlink/core/services/cron"
	"github.com/smartcontractkit/chainlink/core/services/directrequest"
	"github.com/smartcontractkit/chainlink/core/services/feeds"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/keeper"
	"github.com/smartcontractkit/chainlink/core/services/keystore"
	"github.com/smartcontractkit/chainlink/core/services/ocr"
	"github.com/smartcontractkit/chainlink/core/services/ocr2"
	"github.com/smartcontractkit/chainlink/core/services/ocrbootstrap"
	"github.com/smartcontractkit/chainlink/core/services/ocrcommon"
	"github.com/smartcontractkit/chainlink/core/services/periodicbackup"
	"github.com/smartcontractkit/chainlink/core/services/pg"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/core/services/promreporter"
	"github.com/smartcontractkit/chainlink/core/services/relay"
	evmrelay "github.com/smartcontractkit/chainlink/core/services/relay/evm"
	relaytypes "github.com/smartcontractkit/chainlink/core/services/relay/types"
	"github.com/smartcontractkit/chainlink/core/services/synchronization"
	"github.com/smartcontractkit/chainlink/core/services/telemetry"
	"github.com/smartcontractkit/chainlink/core/services/vrf"
	"github.com/smartcontractkit/chainlink/core/services/webhook"
	"github.com/smartcontractkit/chainlink/core/sessions"
	"github.com/smartcontractkit/chainlink/core/utils"
)

//go:generate mockery --name Application --output ../../internal/mocks/ --case=underscore

// Application implements the common functions used in the core node.
type Application interface {
	Start(ctx context.Context) error
	Stop() error
	GetLogger() logger.Logger
	GetHealthChecker() services.Checker
	GetSqlxDB() *sqlx.DB
	GetConfig() config.GeneralConfig
	SetLogLevel(lvl zapcore.Level) error
	GetKeyStore() keystore.Master
	GetEventBroadcaster() pg.EventBroadcaster
	WakeSessionReaper()
	GetWebAuthnConfiguration() sessions.WebAuthnConfiguration

	GetExternalInitiatorManager() webhook.ExternalInitiatorManager
	GetChains() Chains

	// V2 Jobs (TOML specified)
	JobSpawner() job.Spawner
	JobORM() job.ORM
	EVMORM() evmtypes.ORM
	PipelineORM() pipeline.ORM
	BridgeORM() bridges.ORM
	SessionORM() sessions.ORM
	TxmORM() txmgr.ORM
	AddJobV2(ctx context.Context, job *job.Job) error
	DeleteJob(ctx context.Context, jobID int32) error
	RunWebhookJobV2(ctx context.Context, jobUUID uuid.UUID, requestBody string, meta pipeline.JSONSerializable) (int64, error)
	ResumeJobV2(ctx context.Context, taskID uuid.UUID, result pipeline.Result) error
	// Testing only
	RunJobV2(ctx context.Context, jobID int32, meta map[string]interface{}) (int64, error)
	SetServiceLogLevel(ctx context.Context, service string, level zapcore.Level) error

	// Feeds
	GetFeedsService() feeds.Service

	// ReplayFromBlock replays logs from on or after the given block number. If forceBroadcast is
	// set to true, consumers will reprocess data even if it has already been processed.
	ReplayFromBlock(chainID *big.Int, number uint64, forceBroadcast bool) error

	// ID is unique to this particular application instance
	ID() uuid.UUID
}

// ChainlinkApplication contains fields for the JobSubscriber, Scheduler,
// and Store. The JobSubscriber and Scheduler are also available
// in the services package, but the Store has its own package.
type ChainlinkApplication struct {
	Chains                   Chains
	EventBroadcaster         pg.EventBroadcaster
	jobORM                   job.ORM
	jobSpawner               job.Spawner
	pipelineORM              pipeline.ORM
	pipelineRunner           pipeline.Runner
	bridgeORM                bridges.ORM
	sessionORM               sessions.ORM
	txmORM                   txmgr.ORM
	FeedsService             feeds.Service
	webhookJobRunner         webhook.JobRunner
	Config                   config.GeneralConfig
	KeyStore                 keystore.Master
	ExternalInitiatorManager webhook.ExternalInitiatorManager
	SessionReaper            utils.SleeperTask
	shutdownOnce             sync.Once
	explorerClient           synchronization.ExplorerClient
	subservices              []services.ServiceCtx
	HealthChecker            services.Checker
	Nurse                    *services.Nurse
	logger                   logger.Logger
	closeLogger              func() error
	sqlxDB                   *sqlx.DB

	started     bool
	startStopMu sync.Mutex
}

type ApplicationOpts struct {
	Config                   config.GeneralConfig
	EventBroadcaster         pg.EventBroadcaster
	SqlxDB                   *sqlx.DB
	KeyStore                 keystore.Master
	Chains                   Chains
	Logger                   logger.Logger
	CloseLogger              func() error
	ExternalInitiatorManager webhook.ExternalInitiatorManager
	Version                  string
}

// Chains holds a ChainSet for each type of chain.
type Chains struct {
	EVM    evm.ChainSet
	Solana solana.ChainSet // nil if disabled
	Terra  terra.ChainSet  // nil if disabled
}

func (c *Chains) services() (s []services.ServiceCtx) {
	if c.EVM != nil {
		s = append(s, c.EVM)
	}
	if c.Solana != nil {
		s = append(s, c.Solana)
	}
	if c.Terra != nil {
		s = append(s, c.Terra)
	}
	return
}

// NewApplication initializes a new store if one is not already
// present at the configured root directory (default: ~/.chainlink),
// the logger at the same directory and returns the Application to
// be used by the node.
// TODO: Inject more dependencies here to save booting up useless stuff in tests
func NewApplication(opts ApplicationOpts) (Application, error) {
	var subservices []services.ServiceCtx
	db := opts.SqlxDB
	cfg := opts.Config
	keyStore := opts.KeyStore
	chains := opts.Chains
	globalLogger := opts.Logger
	eventBroadcaster := opts.EventBroadcaster
	externalInitiatorManager := opts.ExternalInitiatorManager

	var nurse *services.Nurse
	if cfg.AutoPprofEnabled() {
		globalLogger.Info("Nurse service (automatic pprof profiling) is enabled")
		nurse = services.NewNurse(cfg, globalLogger)
		err := nurse.Start()
		if err != nil {
			return nil, err
		}
	} else {
		globalLogger.Info("Nurse service (automatic pprof profiling) is disabled")
	}

	healthChecker := services.NewChecker()

	telemetryIngressClient := synchronization.TelemetryIngressClient(&synchronization.NoopTelemetryIngressClient{})
	telemetryIngressBatchClient := synchronization.TelemetryIngressBatchClient(&synchronization.NoopTelemetryIngressBatchClient{})
	explorerClient := synchronization.ExplorerClient(&synchronization.NoopExplorerClient{})
	monitoringEndpointGen := telemetry.MonitoringEndpointGenerator(&telemetry.NoopAgent{})

	if cfg.ExplorerURL() != nil && cfg.TelemetryIngressURL() != nil {
		globalLogger.Warn("Both EXPLORER_URL and TELEMETRY_INGRESS_URL are set, defaulting to Explorer")
	}

	if cfg.ExplorerURL() != nil {
		explorerClient = synchronization.NewExplorerClient(cfg.ExplorerURL(), cfg.ExplorerAccessKey(), cfg.ExplorerSecret(), globalLogger)
		monitoringEndpointGen = telemetry.NewExplorerAgent(explorerClient)
	}

	// Use Explorer over TelemetryIngress if both URLs are set
	if cfg.ExplorerURL() == nil && cfg.TelemetryIngressURL() != nil {
		if cfg.TelemetryIngressUseBatchSend() {
			telemetryIngressBatchClient = synchronization.NewTelemetryIngressBatchClient(cfg.TelemetryIngressURL(),
				cfg.TelemetryIngressServerPubKey(), keyStore.CSA(), cfg.TelemetryIngressLogging(), globalLogger, cfg.TelemetryIngressBufferSize(), cfg.TelemetryIngressMaxBatchSize(), cfg.TelemetryIngressSendInterval(), cfg.TelemetryIngressSendTimeout(), cfg.TelemetryIngressUniConn())
			monitoringEndpointGen = telemetry.NewIngressAgentBatchWrapper(telemetryIngressBatchClient)

		} else {
			telemetryIngressClient = synchronization.NewTelemetryIngressClient(cfg.TelemetryIngressURL(),
				cfg.TelemetryIngressServerPubKey(), keyStore.CSA(), cfg.TelemetryIngressLogging(), globalLogger)
			monitoringEndpointGen = telemetry.NewIngressAgentWrapper(telemetryIngressClient)
		}
	}
	subservices = append(subservices, explorerClient, telemetryIngressClient, telemetryIngressBatchClient)

	if cfg.DatabaseBackupMode() != config.DatabaseBackupModeNone && cfg.DatabaseBackupFrequency() > 0 {
		globalLogger.Infow("DatabaseBackup: periodic database backups are enabled", "frequency", cfg.DatabaseBackupFrequency())

		databaseBackup, err := periodicbackup.NewDatabaseBackup(cfg, globalLogger)
		if err != nil {
			return nil, errors.Wrap(err, "NewApplication: failed to initialize database backup")
		}
		subservices = append(subservices, databaseBackup)
	} else {
		globalLogger.Info("DatabaseBackup: periodic database backups are disabled. To enable automatic backups, set DATABASE_BACKUP_MODE=lite or DATABASE_BACKUP_MODE=full")
	}

	subservices = append(subservices, eventBroadcaster)
	subservices = append(subservices, chains.services()...)
	promReporter := promreporter.NewPromReporter(db.DB, globalLogger)
	subservices = append(subservices, promReporter)

	var (
		pipelineORM    = pipeline.NewORM(db, globalLogger, cfg)
		bridgeORM      = bridges.NewORM(db, globalLogger, cfg)
		sessionORM     = sessions.NewORM(db, cfg.SessionTimeout().Duration(), globalLogger)
		pipelineRunner = pipeline.NewRunner(pipelineORM, cfg, chains.EVM, keyStore.Eth(), keyStore.VRF(), globalLogger)
		jobORM         = job.NewORM(db, chains.EVM, pipelineORM, keyStore, globalLogger, cfg)
		txmORM         = txmgr.NewORM(db, globalLogger, cfg)
	)

	for _, chain := range chains.EVM.Chains() {
		chain.HeadBroadcaster().Subscribe(promReporter)
		chain.TxManager().RegisterResumeCallback(pipelineRunner.ResumeRun)
	}

	var (
		delegates = map[job.Type]job.Delegate{
			job.DirectRequest: directrequest.NewDelegate(
				globalLogger,
				pipelineRunner,
				pipelineORM,
				chains.EVM),
			job.Keeper: keeper.NewDelegate(
				db,
				jobORM,
				pipelineRunner,
				globalLogger,
				chains.EVM),
			job.VRF: vrf.NewDelegate(
				db,
				keyStore,
				pipelineRunner,
				pipelineORM,
				chains.EVM,
				globalLogger,
				cfg),
			job.Webhook: webhook.NewDelegate(
				pipelineRunner,
				externalInitiatorManager,
				globalLogger),
			job.Cron: cron.NewDelegate(
				pipelineRunner,
				globalLogger),
			job.BlockhashStore: blockhashstore.NewDelegate(
				globalLogger,
				chains.EVM,
				keyStore.Eth()),
		}
		webhookJobRunner = delegates[job.Webhook].(*webhook.Delegate).WebhookJobRunner()
	)

	// Flux monitor requires ethereum just to boot, silence errors with a null delegate
	if !cfg.EVMRPCEnabled() {
		delegates[job.FluxMonitor] = &job.NullDelegate{Type: job.FluxMonitor}
	} else {
		delegates[job.FluxMonitor] = fluxmonitorv2.NewDelegate(
			keyStore.Eth(),
			jobORM,
			pipelineORM,
			pipelineRunner,
			db,
			chains.EVM,
			globalLogger,
		)
	}

	var peerWrapper *ocrcommon.SingletonPeerWrapper
	if cfg.P2PEnabled() {
		if err := ocrcommon.ValidatePeerWrapperConfig(cfg); err != nil {
			return nil, err
		}
		peerWrapper = ocrcommon.NewSingletonPeerWrapper(keyStore, cfg, db, globalLogger)
		subservices = append(subservices, peerWrapper)
	} else {
		globalLogger.Debug("P2P stack disabled")
	}

	if cfg.FeatureOffchainReporting() {
		delegates[job.OffchainReporting] = ocr.NewDelegate(
			db,
			jobORM,
			keyStore,
			pipelineRunner,
			peerWrapper,
			monitoringEndpointGen,
			chains.EVM,
			globalLogger,
			cfg,
		)
	} else {
		globalLogger.Debug("Off-chain reporting disabled")
	}
	if cfg.FeatureOffchainReporting2() {
		globalLogger.Debug("Off-chain reporting v2 enabled")
		// master/delegate relay is started once, on app start, as root subservice
		relay := relay.NewDelegate(keyStore)
		if cfg.EVMEnabled() {
			evmRelayer := evmrelay.NewRelayer(db, chains.EVM, globalLogger.Named("EVM"))
			relay.AddRelayer(relaytypes.EVM, evmRelayer)
		}
		if cfg.SolanaEnabled() {
			solanaRelayer := pkgsolana.NewRelayer(globalLogger.Named("Solana.Relayer"), chains.Solana)
			solanaRelayerCtx := solanaRelayer
			relay.AddRelayer(relaytypes.Solana, solanaRelayerCtx)
		}
		if cfg.TerraEnabled() {
			terraRelayer := pkgterra.NewRelayer(globalLogger.Named("Terra.Relayer"), chains.Terra)
			terraRelayerCtx := terraRelayer
			relay.AddRelayer(relaytypes.Terra, terraRelayerCtx)
		}
		subservices = append(subservices, relay)
		delegates[job.OffchainReporting2] = ocr2.NewDelegate(
			db,
			jobORM,
			pipelineRunner,
			peerWrapper,
			monitoringEndpointGen,
			chains.EVM,
			globalLogger,
			cfg,
			keyStore.OCR2(),
			relay,
		)
		delegates[job.Bootstrap] = ocrbootstrap.NewDelegateBootstrap(
			db,
			jobORM,
			peerWrapper,
			globalLogger,
			cfg,
			relay,
		)
	} else {
		globalLogger.Debug("Off-chain reporting v2 disabled")
	}

	var lbs []utils.DependentAwaiter
	for _, c := range chains.EVM.Chains() {
		lbs = append(lbs, c.LogBroadcaster())
	}
	jobSpawner := job.NewSpawner(jobORM, cfg, delegates, db, globalLogger, lbs)
	subservices = append(subservices, jobSpawner, pipelineRunner)

	// We start the log poller after the job spawner
	// so jobs have a chance to apply their initial log filters.
	if cfg.FeatureLogPoller() {
		for _, c := range chains.EVM.Chains() {
			subservices = append(subservices, c.LogPoller())
		}
	}

	// TODO: Make feeds manager compatible with multiple chains
	// See: https://app.clubhouse.io/chainlinklabs/story/14615/add-ability-to-set-chain-id-in-all-pipeline-tasks-that-interact-with-evm
	var feedsService feeds.Service
	if cfg.FeatureFeedsManager() {
		feedsORM := feeds.NewORM(db, opts.Logger, cfg)
		chain, err := chains.EVM.Default()
		if err != nil {
			globalLogger.Warnw("Unable to load feeds service; no default chain available", "err", err)
			feedsService = &feeds.NullService{}
		} else {
			feedsService = feeds.NewService(feedsORM, jobORM, db, jobSpawner, keyStore, chain.Config(), chains.EVM, globalLogger, opts.Version)
		}
	} else {
		feedsService = &feeds.NullService{}
	}

	app := &ChainlinkApplication{
		Chains:                   chains,
		EventBroadcaster:         eventBroadcaster,
		jobORM:                   jobORM,
		jobSpawner:               jobSpawner,
		pipelineRunner:           pipelineRunner,
		pipelineORM:              pipelineORM,
		bridgeORM:                bridgeORM,
		sessionORM:               sessionORM,
		txmORM:                   txmORM,
		FeedsService:             feedsService,
		Config:                   cfg,
		webhookJobRunner:         webhookJobRunner,
		KeyStore:                 keyStore,
		SessionReaper:            sessions.NewSessionReaper(db.DB, cfg, globalLogger),
		ExternalInitiatorManager: externalInitiatorManager,
		explorerClient:           explorerClient,
		HealthChecker:            healthChecker,
		Nurse:                    nurse,
		logger:                   globalLogger,
		closeLogger:              opts.CloseLogger,

		sqlxDB: opts.SqlxDB,

		// NOTE: Can keep things clean by putting more things in subservices
		// instead of manually start/closing
		subservices: subservices,
	}

	for _, service := range app.subservices {
		checkable := service.(services.Checkable)
		if err := app.HealthChecker.Register(reflect.TypeOf(service).String(), checkable); err != nil {
			return nil, err
		}
	}

	return app, nil
}

func (app *ChainlinkApplication) SetLogLevel(lvl zapcore.Level) error {
	if err := app.Config.SetLogLevel(lvl); err != nil {
		return err
	}
	app.logger.SetLogLevel(lvl)
	return nil
}

// SetServiceLogLevel sets the Logger level for a given service and stores the setting in the db.
func (app *ChainlinkApplication) SetServiceLogLevel(ctx context.Context, serviceName string, level zapcore.Level) error {
	// TODO: Implement other service loggers
	switch serviceName {
	case logger.HeadTracker:
		for _, c := range app.Chains.EVM.Chains() {
			c.HeadTracker().SetLogLevel(level)
		}
	case logger.FluxMonitor:
		// TODO: Set FMv2?
	case logger.Keeper:
	default:
		return fmt.Errorf("no service found with name: %s", serviceName)
	}

	return logger.NewORM(app.GetSqlxDB(), app.GetLogger()).SetServiceLogLevel(ctx, serviceName, level.String())
}

// Start all necessary services. If successful, nil will be returned.
// Start sequence is aborted if the context gets cancelled.
func (app *ChainlinkApplication) Start(ctx context.Context) error {
	app.startStopMu.Lock()
	defer app.startStopMu.Unlock()
	if app.started {
		panic("application is already started")
	}

	if app.FeedsService != nil {
		if err := app.FeedsService.Start(); err != nil {
			app.logger.Infof("[Feeds Service] %v", err)
		}
	}

	for _, subservice := range app.subservices {
		if ctx.Err() != nil {
			return errors.Wrap(ctx.Err(), "aborting start")
		}

		app.logger.Debugw("Starting service...", "serviceType", reflect.TypeOf(subservice))

		if err := subservice.Start(ctx); err != nil {
			return err
		}
	}

	// Start HealthChecker last, so that the other services had the chance to
	// start enough to immediately pass the readiness check.
	if err := app.HealthChecker.Start(); err != nil {
		return err
	}

	app.started = true

	return nil
}

func (app *ChainlinkApplication) StopIfStarted() error {
	app.startStopMu.Lock()
	defer app.startStopMu.Unlock()
	if app.started {
		return app.stop()
	}
	return nil
}

// Stop allows the application to exit by halting schedules, closing
// logs, and closing the DB connection.
func (app *ChainlinkApplication) Stop() error {
	app.startStopMu.Lock()
	defer app.startStopMu.Unlock()
	return app.stop()
}

func (app *ChainlinkApplication) stop() (err error) {
	if !app.started {
		panic("application is already stopped")
	}
	app.shutdownOnce.Do(func() {
		defer func() {
			if lerr := app.closeLogger(); lerr != nil {
				err = multierr.Append(err, lerr)
			}
		}()
		app.logger.Info("Gracefully exiting...")

		// Stop services in the reverse order from which they were started
		for i := len(app.subservices) - 1; i >= 0; i-- {
			service := app.subservices[i]
			app.logger.Debugw("Closing service...", "serviceType", reflect.TypeOf(service))
			err = multierr.Append(err, service.Close())
		}

		app.logger.Debug("Stopping SessionReaper...")
		err = multierr.Append(err, app.SessionReaper.Stop())
		app.logger.Debug("Closing HealthChecker...")
		err = multierr.Append(err, app.HealthChecker.Close())
		if app.FeedsService != nil {
			app.logger.Debug("Closing Feeds Service...")
			err = multierr.Append(err, app.FeedsService.Close())
		}

		if app.Nurse != nil {
			err = multierr.Append(err, app.Nurse.Close())
		}

		app.logger.Info("Exited all services")

		app.started = false
	})
	return err
}

func (app *ChainlinkApplication) GetConfig() config.GeneralConfig {
	return app.Config
}

func (app *ChainlinkApplication) GetKeyStore() keystore.Master {
	return app.KeyStore
}

func (app *ChainlinkApplication) GetLogger() logger.Logger {
	return app.logger
}

func (app *ChainlinkApplication) GetHealthChecker() services.Checker {
	return app.HealthChecker
}

func (app *ChainlinkApplication) JobSpawner() job.Spawner {
	return app.jobSpawner
}

func (app *ChainlinkApplication) JobORM() job.ORM {
	return app.jobORM
}

func (app *ChainlinkApplication) BridgeORM() bridges.ORM {
	return app.bridgeORM
}

func (app *ChainlinkApplication) SessionORM() sessions.ORM {
	return app.sessionORM
}

func (app *ChainlinkApplication) EVMORM() evmtypes.ORM {
	return app.Chains.EVM.ORM()
}

func (app *ChainlinkApplication) PipelineORM() pipeline.ORM {
	return app.pipelineORM
}

func (app *ChainlinkApplication) TxmORM() txmgr.ORM {
	return app.txmORM
}

func (app *ChainlinkApplication) GetExternalInitiatorManager() webhook.ExternalInitiatorManager {
	return app.ExternalInitiatorManager
}

// WakeSessionReaper wakes up the reaper to do its reaping.
func (app *ChainlinkApplication) WakeSessionReaper() {
	app.SessionReaper.WakeUp()
}

func (app *ChainlinkApplication) AddJobV2(ctx context.Context, j *job.Job) error {
	return app.jobSpawner.CreateJob(j, pg.WithParentCtx(ctx))
}

func (app *ChainlinkApplication) DeleteJob(ctx context.Context, jobID int32) error {
	// Do not allow the job to be deleted if it is managed by the Feeds Manager
	isManaged, err := app.FeedsService.IsJobManaged(ctx, int64(jobID))
	if err != nil {
		return err
	}

	if isManaged {
		return errors.New("job must be deleted in the feeds manager")
	}

	return app.jobSpawner.DeleteJob(jobID, pg.WithParentCtx(ctx))
}

func (app *ChainlinkApplication) RunWebhookJobV2(ctx context.Context, jobUUID uuid.UUID, requestBody string, meta pipeline.JSONSerializable) (int64, error) {
	return app.webhookJobRunner.RunJob(ctx, jobUUID, requestBody, meta)
}

// Only used for local testing, not supported by the UI.
func (app *ChainlinkApplication) RunJobV2(
	ctx context.Context,
	jobID int32,
	meta map[string]interface{},
) (int64, error) {
	if !app.GetConfig().Dev() {
		return 0, errors.New("manual job runs only supported in dev mode - export CHAINLINK_DEV=true to use")
	}
	jb, err := app.jobORM.FindJob(ctx, jobID)
	if err != nil {
		return 0, errors.Wrapf(err, "job ID %v", jobID)
	}
	var runID int64

	// Some jobs are special in that they do not have a task graph.
	isBootstrap := jb.Type == job.OffchainReporting && jb.OCROracleSpec != nil && jb.OCROracleSpec.IsBootstrapPeer
	if jb.Type.RequiresPipelineSpec() || !isBootstrap {
		var vars map[string]interface{}
		var saveTasks bool
		if jb.Type == job.VRF {
			saveTasks = true
			// Create a dummy log to trigger a run
			testLog := types.Log{
				Data: bytes.Join([][]byte{
					jb.VRFSpec.PublicKey.MustHash().Bytes(),  // key hash
					common.BigToHash(big.NewInt(42)).Bytes(), // seed
					utils.NewHash().Bytes(),                  // sender
					utils.NewHash().Bytes(),                  // fee
					utils.NewHash().Bytes()},                 // requestID
					[]byte{}),
				Topics:      []common.Hash{{}, jb.ExternalIDEncodeBytesToTopic()}, // jobID BYTES
				TxHash:      utils.NewHash(),
				BlockNumber: 10,
				BlockHash:   utils.NewHash(),
			}
			vars = map[string]interface{}{
				"jobSpec": map[string]interface{}{
					"databaseID":    jb.ID,
					"externalJobID": jb.ExternalJobID,
					"name":          jb.Name.ValueOrZero(),
					"publicKey":     jb.VRFSpec.PublicKey[:],
				},
				"jobRun": map[string]interface{}{
					"meta":           meta,
					"logBlockHash":   testLog.BlockHash[:],
					"logBlockNumber": testLog.BlockNumber,
					"logTxHash":      testLog.TxHash,
					"logTopics":      testLog.Topics,
					"logData":        testLog.Data,
				},
			}
		} else {
			vars = map[string]interface{}{
				"jobRun": map[string]interface{}{
					"meta": meta,
				},
			}
		}
		runID, _, err = app.pipelineRunner.ExecuteAndInsertFinishedRun(ctx, *jb.PipelineSpec, pipeline.NewVarsFrom(vars), app.logger, saveTasks)
	}
	return runID, err
}

func (app *ChainlinkApplication) ResumeJobV2(
	ctx context.Context,
	taskID uuid.UUID,
	result pipeline.Result,
) error {
	return app.pipelineRunner.ResumeRun(taskID, result.Value, result.Error)
}

func (app *ChainlinkApplication) GetFeedsService() feeds.Service {
	return app.FeedsService
}

// ReplayFromBlock implements the Application interface.
func (app *ChainlinkApplication) ReplayFromBlock(chainID *big.Int, number uint64, forceBroadcast bool) error {
	chain, err := app.Chains.EVM.Get(chainID)
	if err != nil {
		return err
	}
	chain.LogBroadcaster().ReplayFromBlock(int64(number), forceBroadcast)
	return nil
}

// GetChains returns Chains.
func (app *ChainlinkApplication) GetChains() Chains {
	return app.Chains
}

func (app *ChainlinkApplication) GetEventBroadcaster() pg.EventBroadcaster {
	return app.EventBroadcaster
}

func (app *ChainlinkApplication) GetSqlxDB() *sqlx.DB {
	return app.sqlxDB
}

// Returns the configuration to use for creating and authenticating
// new WebAuthn credentials
func (app *ChainlinkApplication) GetWebAuthnConfiguration() sessions.WebAuthnConfiguration {
	rpid := app.Config.RPID()
	rporigin := app.Config.RPOrigin()
	if rpid == "" {
		app.GetLogger().Errorf("RPID is not set, WebAuthn will likely not work as intended")
	}

	if rporigin == "" {
		app.GetLogger().Errorf("RPOrigin is not set, WebAuthn will likely not work as intended")
	}

	return sessions.WebAuthnConfiguration{
		RPID:     rpid,
		RPOrigin: rporigin,
	}
}

func (app *ChainlinkApplication) ID() uuid.UUID {
	return app.Config.AppID()
}
