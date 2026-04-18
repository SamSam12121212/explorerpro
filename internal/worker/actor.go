package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docprompt"
	"explorer/internal/docstore"
	"explorer/internal/eventrelay"
	"explorer/internal/idgen"
	"explorer/internal/openaiws"
	"explorer/internal/preparedinput"
	"explorer/internal/threadcmd"
	"explorer/internal/threadcollectionstore"
	"explorer/internal/threadhistory"
	"explorer/internal/threadstore"

	"github.com/nats-io/nats.go"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

var (
	errOwnershipConflict   = errors.New("thread is owned by another worker")
	errCommandPrecond      = errors.New("command precondition failed")
	errRemotePermanent     = errors.New("openai returned a permanent error")
	errUnsupportedKind     = errors.New("unsupported command kind")
	errSocketBudgetExhausted = errors.New("openai socket budget exhausted")
)

const (
	maxTransientCommandDeliveries = 5
	transientCommandRetryBase     = 2 * time.Second
	transientCommandRetryMax      = 30 * time.Second
	// maxChildrenPerSpawn caps the number of children a single spawn_threads
	// or query_document round can launch. Each child gets its own OpenAI
	// socket, so this combined with the process-wide socket budget is what
	// prevents a buggy turn from torching the OpenAI quota.
	maxChildrenPerSpawn = 50
)

type threadActorConfig struct {
	ThreadID          int64
	WorkerID          int64
	Logger            *slog.Logger
	Store             actorStore
	History           threadHistoryStore
	ThreadDocs        threadDocumentStore
	ThreadCollections threadCollectionStore
	DocRuntime        docRuntimeContextClient
	DocStore          docActorDocStore
	PreparedInputs    docActorPreparedInputClient
	Blob              *blobstore.LocalStore
	OpenAIConfig      openaiws.Config
	Publish           func(ctx context.Context, subject string, cmd threadcmd.Command) error
	PublishEvent      func(ctx context.Context, threadID int64, socketGeneration uint64, key string, eventType string, raw json.RawMessage) error
	SessionFactory    func() *openaiws.Session
	BudgetReserve     func() error
	BudgetRelease     func()
}

type actorStore interface {
	ReserveThreadID(ctx context.Context) (int64, error)
	ReserveSpawnGroupID(ctx context.Context) (int64, error)
	ReserveCommandID(ctx context.Context, bus, kind string) (int64, error)
	LoadOrCreateCommandID(ctx context.Context, bus, kind, dedupeKey string) (int64, error)
	CreateThreadIfAbsent(ctx context.Context, meta threadstore.ThreadMeta) error
	LoadThread(ctx context.Context, threadID int64) (threadstore.ThreadMeta, error)
	LoadReusableDocumentChildThread(ctx context.Context, parentThreadID, documentID int64) (threadstore.ThreadMeta, error)
	LoadLatestCompletedDocumentQueryLineage(ctx context.Context, parentThreadID int64, documentID int64) (threadstore.DocumentQueryLineage, error)
	SaveThread(ctx context.Context, meta threadstore.ThreadMeta) error
	CommandProcessed(ctx context.Context, threadID int64, cmdID int64) (bool, error)
	MarkCommandProcessed(ctx context.Context, threadID int64, cmdID int64) (bool, error)
	ClaimOwnership(ctx context.Context, threadID int64, workerID int64, leaseUntil time.Time) (threadstore.ClaimResult, error)
	RenewOwnership(ctx context.Context, threadID int64, workerID int64, socketGeneration uint64, leaseUntil time.Time) (bool, error)
	RotateOwnership(ctx context.Context, threadID int64, workerID int64, currentGeneration uint64, leaseUntil, socketExpiresAt time.Time) (uint64, bool, error)
	ReleaseOwnership(ctx context.Context, threadID int64, workerID int64, socketGeneration uint64) error
	CreateOpenAISocketSession(ctx context.Context, session threadstore.OpenAISocketSession) error
	TouchOpenAISocketSession(ctx context.Context, touch threadstore.OpenAISocketTouch) error
	DisconnectOpenAISocketSession(ctx context.Context, socketID, reason string, disconnectedAt, expiresAt time.Time) error
	AppendItem(ctx context.Context, entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error)
	SaveResponseRaw(ctx context.Context, threadID int64, responseID string, payload json.RawMessage) error
	ListItems(ctx context.Context, threadID int64, options threadstore.ListOptions) ([]threadstore.ItemRecord, error)
	LoadOrCreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) (threadstore.SpawnGroupMeta, error)
	CreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta, childThreadIDs []int64) error
	LoadSpawnGroup(ctx context.Context, spawnGroupID int64) (threadstore.SpawnGroupMeta, error)
	SaveSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) error
	ListSpawnResults(ctx context.Context, spawnGroupID int64) ([]threadstore.SpawnChildResult, error)
	UpsertSpawnResult(ctx context.Context, spawnGroupID int64, result threadstore.SpawnChildResult) (bool, []threadstore.SpawnChildResult, error)
}

type threadHistoryStore interface {
	SaveResponseCreateCheckpoint(ctx context.Context, threadID int64, checkpointID string, payload json.RawMessage) error
	LoadLatestResponseCreateCheckpoint(ctx context.Context, threadID int64) (json.RawMessage, error)
	AppendEvent(ctx context.Context, entry threadstore.EventLogEntry, eventID string) error
}

type threadDocumentStore interface {
	ListDocuments(ctx context.Context, threadID int64, limit int64) ([]docstore.Document, error)
	FilterAttached(ctx context.Context, threadID int64, documentIDs []int64) ([]int64, error)
}

type threadCollectionStore interface {
	ListAttached(ctx context.Context, threadID int64, limit int64) ([]threadcollectionstore.AttachedCollection, error)
}

type docActorDocStore interface {
	Get(ctx context.Context, id int64) (docstore.Document, error)
	UpdateBaseLineage(ctx context.Context, id int64, baseResponseID, baseModel, baseReasoning string) error
}

type docActorPreparedInputClient interface {
	PrepareInput(ctx context.Context, req doccmd.PrepareInputRequest) (doccmd.PrepareInputResponse, error)
}

type threadActor struct {
	threadID          int64
	workerID          int64
	logger            *slog.Logger
	store             actorStore
	history           threadHistoryStore
	threadDocs        threadDocumentStore
	threadCollections threadCollectionStore
	docRuntime        docRuntimeContextClient
	docStore          docActorDocStore
	preparedInputs    docActorPreparedInputClient
	blob              *blobstore.LocalStore
	cfg               openaiws.Config
	publish           func(ctx context.Context, subject string, cmd threadcmd.Command) error
	publishEvent      func(ctx context.Context, threadID int64, socketGeneration uint64, key string, eventType string, raw json.RawMessage) error

	sessionFactory func() *openaiws.Session
	budgetReserve  func() error
	budgetRelease  func()

	ctx    context.Context
	cancel context.CancelFunc

	commands chan queuedCommand
	done     chan struct{}

	mu             sync.Mutex
	session        *openaiws.Session
	openAISocketID string
	leaseCancel    context.CancelFunc
	idleCancel     context.CancelFunc
	idleDone       chan struct{}
	meta           threadstore.ThreadMeta
}

type payloadLoweringStats struct {
	InputItemsCount    int
	LoweredImageInputs int
	LoweredBlobRefs    int
}

type docRuntimeContextClient interface {
	RuntimeContext(ctx context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error)
}

type deltaLogState struct {
	firstRaw      string
	lastRaw       string
	firstLogged   bool
	suppressedAny bool
}

func newThreadActor(parentCtx context.Context, cfg threadActorConfig) *threadActor {
	ctx, cancel := context.WithCancel(parentCtx)

	actor := &threadActor{
		threadID:          cfg.ThreadID,
		workerID:          cfg.WorkerID,
		logger:            cfg.Logger,
		store:             cfg.Store,
		history:           cfg.History,
		threadDocs:        cfg.ThreadDocs,
		threadCollections: cfg.ThreadCollections,
		docRuntime:        cfg.DocRuntime,
		docStore:          cfg.DocStore,
		preparedInputs:    cfg.PreparedInputs,
		blob:              cfg.Blob,
		cfg:               cfg.OpenAIConfig,
		publish:           cfg.Publish,
		publishEvent:      cfg.PublishEvent,
		sessionFactory:    cfg.SessionFactory,
		budgetReserve:     cfg.BudgetReserve,
		budgetRelease:     cfg.BudgetRelease,
		ctx:               ctx,
		cancel:            cancel,
		commands:          make(chan queuedCommand, commandQueueSize),
		done:              make(chan struct{}),
	}

	go actor.run()
	return actor
}

func (a *threadActor) Enqueue(cmd queuedCommand) bool {
	select {
	case <-a.ctx.Done():
		return false
	case a.commands <- cmd:
		return true
	}
}

func (a *threadActor) IsClosed() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

func (a *threadActor) Close() error {
	a.cancel()
	<-a.done

	var errs []error

	a.mu.Lock()
	if a.leaseCancel != nil {
		a.leaseCancel()
		a.leaseCancel = nil
	}
	if a.idleCancel != nil {
		a.idleCancel()
		a.idleCancel = nil
	}
	idleDone := a.idleDone
	session := a.session
	socketID := a.openAISocketID
	meta := a.meta
	a.session = nil
	a.openAISocketID = ""
	a.mu.Unlock()

	if idleDone != nil {
		<-idleDone
	}

	if session != nil {
		errs = append(errs, a.closeAndRelease(session))
	}
	a.disconnectOpenAISocket(socketID, "actor_close")

	if meta.ID > 0 && meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		errs = append(errs, a.store.ReleaseOwnership(context.Background(), meta.ID, a.workerID, meta.SocketGeneration))
	}

	return errors.Join(errs...)
}

func (a *threadActor) run() {
	// Defers run LIFO. Order matters: cancel ctx FIRST so Enqueue starts
	// returning false (and the dispatcher NACKs the message back to
	// JetStream for re-delivery) before IsClosed() flips and getActor
	// thinks the actor is gone.
	defer close(a.done)
	defer a.cancel()

	for {
		select {
		case <-a.ctx.Done():
			return
		case queued, ok := <-a.commands:
			if !ok {
				return
			}

			if err := a.handleQueuedCommand(queued); err != nil {
				a.logger.Error("thread actor failed command",
					"cmd_id", queued.cmd.CmdID,
					"kind", queued.cmd.Kind,
					"error", err,
				)
			}

			if a.shouldSelfDestruct() {
				a.logger.Info("child actor self-destructing after terminal status",
					"thread_id", a.threadID,
					"status", a.currentMeta().Status,
				)
				return
			}
		}
	}
}

// shouldSelfDestruct returns true when the actor has finished its work and
// can release its goroutine + map slot. Only child threads that have reached
// a terminal status qualify — root threads stay alive between turns so the
// next user message reuses the same actor (and any warm doc-query children
// remain reusable on their own actors).
func (a *threadActor) shouldSelfDestruct() bool {
	meta := a.currentMeta()
	return meta.ParentThreadID > 0 && isTerminalThreadStatus(meta.Status)
}

func (a *threadActor) handleQueuedCommand(queued queuedCommand) error {
	alreadyProcessed, err := a.store.CommandProcessed(a.ctx, queued.cmd.ThreadID, queued.cmd.CmdID)
	if err != nil {
		_ = queued.msg.Nak()
		return err
	}

	if alreadyProcessed {
		a.logger.Info("skipping already-processed command",
			"cmd_id", queued.cmd.CmdID,
			"kind", queued.cmd.Kind,
		)
		return queued.msg.Ack()
	}

	a.logger.Info("processing command",
		a.appendCommandLifecycleGraphAttrs([]any{
			"cmd_id", queued.cmd.CmdID,
			"kind", queued.cmd.Kind,
		}, queued.cmd)...,
	)

	if err := a.processCommand(queued.cmd); err != nil {
		switch {
		case errors.Is(err, errOwnershipConflict):
			a.logger.Warn("ignoring command for foreign owner",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
			)
			return queued.msg.Ack()
		case errors.Is(err, errCommandPrecond):
			a.logger.Warn("dropping command that failed thread preconditions",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
				"error", err,
			)
			return queued.msg.Ack()
		case shouldDropMissingThreadCommand(err):
			a.logger.Info("dropping command for missing thread",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
				"error", err,
			)
			return queued.msg.Ack()
		case errors.Is(err, errUnsupportedKind):
			a.logger.Error("terminating unsupported command",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
			)
			return queued.msg.Term()
		case errors.Is(err, errRemotePermanent):
			a.logger.Warn("dropping command after permanent openai error",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
				"error", err,
			)
			return queued.msg.Ack()
		default:
			if isTransientCommandError(err) {
				return a.handleTransientCommandError(queued, err)
			}
			_ = queued.msg.Nak()
			return err
		}
	}

	if _, err := a.store.MarkCommandProcessed(a.ctx, queued.cmd.ThreadID, queued.cmd.CmdID); err != nil {
		_ = queued.msg.Nak()
		return err
	}

	a.logger.Info("command completed",
		a.appendCommandLifecycleGraphAttrs([]any{
			"cmd_id", queued.cmd.CmdID,
			"kind", queued.cmd.Kind,
		}, queued.cmd)...,
	)

	return queued.msg.Ack()
}

func (a *threadActor) appendCommandLifecycleGraphAttrs(attrs []any, cmd threadcmd.Command) []any {
	meta := a.commandLifecycleThreadMeta(cmd)
	return appendThreadGraphAttrs(attrs, meta)
}

func (a *threadActor) commandLifecycleThreadMeta(cmd threadcmd.Command) threadstore.ThreadMeta {
	fallback := threadstore.ThreadMeta{
		ID:           cmd.ThreadID,
		RootThreadID: cmd.RootThreadID,
	}

	a.mu.Lock()
	current := a.meta
	a.mu.Unlock()
	if current.ID == cmd.ThreadID {
		if current.RootThreadID <= 0 {
			current.RootThreadID = defaultRootThreadID(fallback)
		}
		return current
	}

	if a.store == nil {
		return fallback
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return fallback
	}
	if meta.RootThreadID <= 0 {
		meta.RootThreadID = defaultRootThreadID(fallback)
	}
	return meta
}

func (a *threadActor) handleTransientCommandError(queued queuedCommand, cause error) error {
	deliveries := commandDeliveries(queued.msg)
	if deliveries >= maxTransientCommandDeliveries {
		a.logger.Warn("dropping command after transient retry exhaustion",
			"cmd_id", queued.cmd.CmdID,
			"kind", queued.cmd.Kind,
			"deliveries", deliveries,
			"max_deliveries", maxTransientCommandDeliveries,
			"error", cause,
		)

		if err := a.failThreadAfterRetryExhaustion(queued.cmd.ThreadID); err != nil {
			_ = queued.msg.NakWithDelay(transientCommandRetryDelay(deliveries))
			return errors.Join(cause, err)
		}

		return queued.msg.Ack()
	}

	delay := transientCommandRetryDelay(deliveries)
	a.logger.Warn("transient command failure, delaying retry",
		"cmd_id", queued.cmd.CmdID,
		"kind", queued.cmd.Kind,
		"deliveries", deliveries,
		"max_deliveries", maxTransientCommandDeliveries,
		"retry_in", delay,
		"error", cause,
	)

	return queued.msg.NakWithDelay(delay)
}

func (a *threadActor) failThreadAfterRetryExhaustion(threadID int64) error {
	meta, err := a.store.LoadThread(a.ctx, threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return nil
		}
		return err
	}

	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession("retry_exhausted")

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
		meta.OwnerWorkerID = 0
		meta.SocketExpiresAt = time.Time{}
	}

	meta.Status = threadstore.ThreadStatusFailed
	meta.ActiveResponseID = ""
	meta.UpdatedAt = time.Now().UTC()

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}
	return nil
}

func commandDeliveries(msg *nats.Msg) uint64 {
	if msg == nil {
		return 1
	}

	metadata, err := msg.Metadata()
	if err != nil || metadata == nil || metadata.NumDelivered == 0 {
		return 1
	}

	return metadata.NumDelivered
}

func transientCommandRetryDelay(deliveries uint64) time.Duration {
	if deliveries == 0 {
		deliveries = 1
	}

	shift := deliveries - 1
	if shift > 4 {
		shift = 4
	}

	delay := transientCommandRetryBase << shift
	if delay > transientCommandRetryMax {
		return transientCommandRetryMax
	}

	return delay
}

func isTransientCommandError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	lower := strings.ToLower(err.Error())
	for _, needle := range []string{
		"no such host",
		"temporary failure in name resolution",
		"server misbehaving",
		"network is unreachable",
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"tls handshake timeout",
		"i/o timeout",
		"dial tcp",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}

	return false
}

func shouldDropMissingThreadCommand(err error) bool {
	return errors.Is(err, threadstore.ErrThreadNotFound)
}

func (a *threadActor) processCommand(cmd threadcmd.Command) error {
	switch cmd.Kind {
	case threadcmd.KindThreadStart:
		return a.handleStart(cmd)
	case threadcmd.KindThreadResume:
		return a.handleResume(cmd)
	case threadcmd.KindThreadSubmitToolOutput:
		return a.handleSubmitToolOutput(cmd)
	case threadcmd.KindThreadChildCompleted:
		return a.handleChildResult(cmd, "completed")
	case threadcmd.KindThreadChildFailed:
		return a.handleChildResult(cmd, "failed")
	case threadcmd.KindThreadAdopt:
		return a.handleAdopt(cmd)
	case threadcmd.KindThreadRotateSocket:
		return a.handleRotateSocket(cmd)
	case threadcmd.KindThreadReconcile:
		return a.handleReconcile(cmd)
	case threadcmd.KindThreadCancel:
		return a.handleCancel(cmd)
	case threadcmd.KindThreadDisconnectSocket:
		return a.handleDisconnectSocket(cmd)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedKind, cmd.Kind)
	}
}

func (a *threadActor) handleStart(cmd threadcmd.Command) error {
	body, err := cmd.StartBody()
	if err != nil {
		return err
	}

	if strings.TrimSpace(body.PreviousResponseID) != "" && !boolOrDefault(body.Store, true) {
		return fmt.Errorf("thread.start previous_response_id requires store=true")
	}
	if !isBlankInputJSON(body.InitialInput) {
		body.InitialInput, err = normalizeInputItems(body.InitialInput)
		if err != nil {
			return fmt.Errorf("normalize thread.start initial_input: %w", err)
		}
	} else {
		body.InitialInput = nil
	}
	body.Metadata, err = normalizeMetadataJSON(body.Metadata)
	if err != nil {
		return fmt.Errorf("normalize thread.start metadata: %w", err)
	}
	body.Include, err = threadcmd.NormalizeInclude(body.Include)
	if err != nil {
		return fmt.Errorf("normalize thread.start include: %w", err)
	}
	body.Tools, err = threadcmd.NormalizeTools(body.Tools)
	if err != nil {
		return fmt.Errorf("normalize thread.start tools: %w", err)
	}
	body.ToolChoice, err = threadcmd.NormalizeToolChoice(body.ToolChoice)
	if err != nil {
		return fmt.Errorf("normalize thread.start tool_choice: %w", err)
	}
	body.Reasoning, err = threadcmd.NormalizeReasoning(body.Reasoning)
	if err != nil {
		return fmt.Errorf("normalize thread.start reasoning: %w", err)
	}

	now := time.Now().UTC()
	if err := a.store.CreateThreadIfAbsent(a.ctx, threadstore.ThreadMeta{
		ID:             cmd.ThreadID,
		RootThreadID:   cmd.RootThreadID,
		Status:         threadstore.ThreadStatusNew,
		Model:          body.Model,
		Instructions:   body.Instructions,
		MetadataJSON:   string(body.Metadata),
		IncludeJSON:    string(body.Include),
		ToolsJSON:      string(body.Tools),
		ToolChoiceJSON: string(body.ToolChoice),
		ReasoningJSON:  string(body.Reasoning),
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	startKind := "root"
	if meta.ParentThreadID > 0 {
		startKind = "child"
	}

	a.logger.Info("starting thread",
		appendThreadGraphAttrs([]any{
			"cmd_id", cmd.CmdID,
			"model", body.Model,
			"start_kind", startKind,
			"input_kind", responseCreateInputKind(map[string]any{
				"input":              body.InitialInput,
				"prepared_input_ref": body.PreparedInputRef,
			}),
			"has_previous_response_id", strings.TrimSpace(body.PreviousResponseID) != "",
		}, meta)...,
	)

	claim, err := a.store.ClaimOwnership(a.ctx, cmd.ThreadID, a.workerID, now.Add(workerLeaseTTL))
	if err != nil {
		return err
	}
	if !claim.Claimed {
		return errOwnershipConflict
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = claim.SocketGeneration
	meta.SocketExpiresAt = now.Add(socketExpiryTTL)
	meta.Status = threadstore.ThreadStatusRunning
	meta.Model = body.Model
	meta.Instructions = body.Instructions
	meta.MetadataJSON = string(body.Metadata)
	meta.IncludeJSON = string(body.Include)
	meta.ToolsJSON = string(body.Tools)
	meta.ToolChoiceJSON = string(body.ToolChoice)
	meta.ReasoningJSON = string(body.Reasoning)
	meta.UpdatedAt = now

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}
	a.startLeaseLoop(meta)
	a.stopIdleLoop()

	if err := a.ensureSession(); err != nil {
		return err
	}

	inputResponseID := ""
	if strings.TrimSpace(body.PreviousResponseID) != "" {
		inputResponseID = body.PreviousResponseID
	}
	if !isBlankInputJSON(body.InitialInput) {
		if err := a.appendInputItems(meta.ID, inputResponseID, body.InitialInput); err != nil {
			return err
		}
	}

	payload, err := a.buildThreadResponseCreatePayload(meta, map[string]any{
		"model":                body.Model,
		"instructions":         body.Instructions,
		"input":                body.InitialInput,
		"metadata":             body.Metadata,
		"include":              body.Include,
		"tools":                body.Tools,
		"tool_choice":          body.ToolChoice,
		"reasoning":            body.Reasoning,
		"store":                boolOrDefault(body.Store, true),
		"previous_response_id": body.PreviousResponseID,
		"prepared_input_ref":   body.PreparedInputRef,
	})
	if err != nil {
		return err
	}

	return a.sendAndStream(meta, cmd.CmdID, payload, "start")
}

func (a *threadActor) handleResume(cmd threadcmd.Command) error {
	body, err := cmd.ResumeBody()
	if err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	claim, err := a.store.ClaimOwnership(a.ctx, meta.ID, a.workerID, time.Now().UTC().Add(workerLeaseTTL))
	if err != nil {
		return err
	}
	if !claim.Claimed {
		return errOwnershipConflict
	}

	if meta.LastResponseID == "" {
		return fmt.Errorf("%w: thread %d has no last_response_id for resume", errCommandPrecond, meta.ID)
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = claim.SocketGeneration
	meta.SocketExpiresAt = time.Now().UTC().Add(socketExpiryTTL)
	meta.Status = threadstore.ThreadStatusRunning
	meta.UpdatedAt = time.Now().UTC()
	if len(body.Reasoning) > 0 {
		meta.ReasoningJSON = string(body.Reasoning)
	}

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}
	a.startLeaseLoop(meta)
	a.stopIdleLoop()

	a.logger.Info("resuming thread",
		appendThreadGraphAttrs([]any{
			"cmd_id", cmd.CmdID,
			"resume_reason", "user_input",
			"input_kind", responseCreateInputKind(map[string]any{
				"input":              body.InputItems,
				"prepared_input_ref": body.PreparedInputRef,
			}),
		}, meta)...,
	)

	if err := a.ensureSession(); err != nil {
		return err
	}

	if !isBlankInputJSON(body.InputItems) {
		if err := a.appendInputItems(meta.ID, meta.LastResponseID, body.InputItems); err != nil {
			return err
		}
	}

	return a.continueWithPreparedInput(meta, cmd.CmdID, body.InputItems, body.PreparedInputRef, "user_input")
}

func (a *threadActor) handleSubmitToolOutput(cmd threadcmd.Command) error {
	body, err := cmd.SubmitToolOutputBody()
	if err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	if meta.LastResponseID == "" {
		return fmt.Errorf("%w: thread %d has no last_response_id for tool output", errCommandPrecond, meta.ID)
	}

	if meta.Status != threadstore.ThreadStatusWaitingTool && meta.Status != threadstore.ThreadStatusReady {
		return fmt.Errorf("thread %d is not ready for tool output, current status=%s", meta.ID, meta.Status)
	}

	claim, err := a.store.ClaimOwnership(a.ctx, meta.ID, a.workerID, time.Now().UTC().Add(workerLeaseTTL))
	if err != nil {
		return err
	}
	if !claim.Claimed {
		return errOwnershipConflict
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = claim.SocketGeneration
	meta.SocketExpiresAt = time.Now().UTC().Add(socketExpiryTTL)
	meta.Status = threadstore.ThreadStatusRunning
	meta.UpdatedAt = time.Now().UTC()

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}
	a.startLeaseLoop(meta)
	a.stopIdleLoop()

	a.logger.Info("resuming thread",
		appendThreadGraphAttrs([]any{
			"cmd_id", cmd.CmdID,
			"resume_reason", "tool_output",
			"input_kind", "function_call_output",
		}, meta)...,
	)

	if err := a.ensureSession(); err != nil {
		return err
	}

	inputItems, err := wrapRawItemAsArray(body.OutputItem)
	if err != nil {
		return err
	}

	if err := a.appendInputItems(meta.ID, meta.LastResponseID, inputItems); err != nil {
		return err
	}

	return a.continueWithInputItems(meta, cmd.CmdID, inputItems, "tool_output")
}

func (a *threadActor) handleChildResult(cmd threadcmd.Command, fallbackStatus string) error {
	body, err := cmd.ChildResultBody()
	if err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	if meta.Status != threadstore.ThreadStatusWaitingChildren {
		return fmt.Errorf("%w: thread %d is not waiting on children, current status=%s", errCommandPrecond, meta.ID, meta.Status)
	}

	spawnGroupID := body.SpawnGroupID
	if meta.ActiveSpawnGroupID > 0 && meta.ActiveSpawnGroupID != spawnGroupID {
		return fmt.Errorf("thread %d active spawn group mismatch: meta=%d cmd=%d", meta.ID, meta.ActiveSpawnGroupID, spawnGroupID)
	}

	spawn, err := a.store.LoadSpawnGroup(a.ctx, spawnGroupID)
	if err != nil {
		return err
	}

	status := body.Status
	if status == "" {
		status = fallbackStatus
	}

	warmupHandled, err := a.handleDocumentWarmupChildResult(meta, spawn, body, status)
	if err != nil {
		return err
	}
	if warmupHandled {
		a.startIdleLoop(meta)
		return nil
	}

	stored, results, err := a.store.UpsertSpawnResult(a.ctx, spawnGroupID, threadstore.SpawnChildResult{
		ChildThreadID:    body.ChildThreadID,
		Status:           status,
		ChildResponseID:  body.ChildResponseID,
		AssistantText:    body.AssistantText,
		ResultRef:        body.ResultRef,
		SummaryRef:       body.SummaryRef,
		ErrorRef:         body.ErrorRef,
		ToolCallArgsJSON: body.ToolCallArgsJSON,
		UpdatedAt:        time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if !stored && !spawn.AggregateSubmittedAt.IsZero() {
		return nil
	}

	spawn.Completed, spawn.Failed, spawn.Cancelled = summarizeSpawnResults(results)
	if spawn.Completed+spawn.Failed+spawn.Cancelled >= spawn.Expected {
		spawn.Status = threadstore.SpawnGroupStatusClosed
	} else if spawn.Status == "" {
		spawn.Status = threadstore.SpawnGroupStatusWaiting
	}
	spawn.UpdatedAt = time.Now().UTC()

	a.logger.Info("child barrier updated",
		appendThreadGraphAttrs([]any{
			"cmd_id", cmd.CmdID,
			"spawn_group_id", spawnGroupID,
			"child_thread_id", body.ChildThreadID,
			"child_status", status,
			"child_response_id", body.ChildResponseID,
			"completed_children", spawn.Completed,
			"failed_children", spawn.Failed,
			"cancelled_children", spawn.Cancelled,
			"expected_children", spawn.Expected,
		}, meta)...,
	)

	if spawn.AggregateSubmittedAt.IsZero() && spawn.Completed+spawn.Failed+spawn.Cancelled >= spawn.Expected {
		outputItem, err := a.aggregateSpawnOutputItemForGroup(spawn, results)
		if err != nil {
			return err
		}

		spawn.AggregateSubmittedAt = time.Now().UTC()
		spawn.AggregateCmdID = cmd.CmdID
		if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
			return err
		}

		inputItems, err := normalizeRawItemsAsArray(outputItem)
		if err != nil {
			return err
		}

		claim, err := a.store.ClaimOwnership(a.ctx, cmd.ThreadID, a.workerID, time.Now().UTC().Add(workerLeaseTTL))
		if err != nil {
			return err
		}
		if !claim.Claimed {
			return errOwnershipConflict
		}

		meta.OwnerWorkerID = a.workerID
		meta.SocketGeneration = claim.SocketGeneration
		meta.SocketExpiresAt = time.Now().UTC().Add(socketExpiryTTL)
		meta.Status = threadstore.ThreadStatusRunning
		meta.UpdatedAt = time.Now().UTC()

		if err := a.saveThreadMeta(meta); err != nil {
			return err
		}
		a.startLeaseLoop(meta)
		a.stopIdleLoop()

		a.logger.Info("resuming thread",
			appendThreadGraphAttrs([]any{
				"cmd_id", cmd.CmdID,
				"resume_reason", "child_barrier",
				"input_kind", "function_call_output",
				"completed_children", spawn.Completed,
				"failed_children", spawn.Failed,
				"cancelled_children", spawn.Cancelled,
				"expected_children", spawn.Expected,
			}, meta)...,
		)

		if err := a.ensureSession(); err != nil {
			return err
		}

		if err := a.appendInputItems(meta.ID, meta.LastResponseID, inputItems); err != nil {
			return err
		}

		return a.continueWithInputItems(meta, cmd.CmdID, inputItems, "child_barrier")
	}

	if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
		return err
	}

	a.startIdleLoop(meta)
	return nil
}

func (a *threadActor) handleDocumentWarmupChildResult(parentMeta threadstore.ThreadMeta, spawn threadstore.SpawnGroupMeta, body threadcmd.ChildResultBody, status string) (bool, error) {
	if status != "completed" || body.ChildThreadID <= 0 {
		return false, nil
	}

	childMeta, err := a.store.LoadThread(a.ctx, body.ChildThreadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return false, nil
		}
		return false, err
	}

	childMetadata, err := decodeThreadMetadataJSON(childMeta.MetadataJSON)
	if err != nil {
		return false, fmt.Errorf("decode child metadata for %d: %w", body.ChildThreadID, err)
	}
	if childMetadata["spawn_mode"] != "document_warmup" {
		return false, nil
	}
	if strings.TrimSpace(body.ChildResponseID) == "" {
		return false, fmt.Errorf("document warmup child %d completed without child_response_id", body.ChildThreadID)
	}
	if a.docStore == nil {
		return false, fmt.Errorf("document store not available")
	}

	documentID := strings.TrimSpace(childMetadata["document_id"])
	if documentID == "" {
		return false, fmt.Errorf("document warmup child %d missing document_id metadata", body.ChildThreadID)
	}
	documentIDValue, err := parseMetadataDocumentID(documentID)
	if err != nil {
		return false, fmt.Errorf("document warmup child %d has invalid document_id metadata %q: %w", body.ChildThreadID, documentID, err)
	}
	documentTask := strings.TrimSpace(childMetadata["document_task"])
	if documentTask == "" {
		return false, fmt.Errorf("document warmup child %d missing document_task metadata", body.ChildThreadID)
	}
	parentCallID := strings.TrimSpace(childMeta.ParentCallID)
	if parentCallID == "" {
		parentCallID = primaryDocQueryRoundCallID(spawn.ParentCallID)
	}
	model := defaultString(strings.TrimSpace(childMeta.Model), strings.TrimSpace(parentMeta.Model))
	reasoning := extractReasoningEffort(defaultString(childMeta.ReasoningJSON, parentMeta.ReasoningJSON))

	if err := a.docStore.UpdateBaseLineage(a.ctx, documentIDValue, body.ChildResponseID, model, reasoning); err != nil {
		return false, fmt.Errorf("update document base lineage for %s: %w", documentID, err)
	}

	queryThreadID, startCmd, reusedThread, err := a.buildDocumentChildStartCommand(
		parentMeta,
		spawn.ID,
		parentCallID,
		documentIDValue,
		childMetadata["document_name"],
		model,
		reasoning,
		"query",
		body.ChildResponseID,
		"",
		documentTask,
		body.ChildThreadID,
	)
	if err != nil {
		return false, err
	}

	if err := a.publish(a.ctx, threadcmd.DispatchStartSubject, startCmd); err != nil {
		return false, err
	}

	a.logger.Info("spawning child thread",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawn.ID,
			"child_thread_id", queryThreadID,
			"bootstrap_child_thread_id", body.ChildThreadID,
			"child_kind", "document_query",
			"lineage_source", "warmup",
			"document_id", documentID,
			"document_name", childMetadata["document_name"],
			"phase", "query",
			"model", model,
			"has_previous_response_id", true,
			"reuse_existing_thread", reusedThread,
		}, parentMeta)...,
	)
	return true, nil
}

func (a *threadActor) handleAdopt(cmd threadcmd.Command) error {
	if _, err := cmd.AdoptBody(); err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	return a.recoverThread(meta, cmd.CmdID, false)
}

func (a *threadActor) handleRotateSocket(cmd threadcmd.Command) error {
	body, err := cmd.RotateSocketBody()
	if err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}
	if !statusSupportsIdleSocket(meta.Status) {
		return fmt.Errorf("%w: thread %d status %s is not safe for rotation", errCommandPrecond, meta.ID, meta.Status)
	}

	a.stopIdleLoop()
	a.stopLeaseLoop()

	freshSession, err := a.openFreshSession()
	if err != nil {
		a.startLeaseLoop(meta)
		a.startIdleLoop(meta)
		return err
	}

	now := time.Now().UTC()
	newGeneration, rotated, err := a.store.RotateOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration, now.Add(workerLeaseTTL), now.Add(socketExpiryTTL))
	if err != nil {
		_ = a.closeAndRelease(freshSession)
		a.startLeaseLoop(meta)
		a.startIdleLoop(meta)
		return err
	}
	if !rotated {
		_ = a.closeAndRelease(freshSession)
		a.handleLeaseLoss(meta.SocketGeneration)
		return errOwnershipConflict
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = newGeneration
	meta.SocketExpiresAt = now.Add(socketExpiryTTL)
	meta.UpdatedAt = now
	a.setMeta(meta)

	socketID := a.registerOpenAISocket(meta, freshSession, now.Add(workerLeaseTTL))
	oldSession, oldSocketID := a.swapSessionState(freshSession, socketID)
	if oldSession != nil {
		if err := a.closeAndRelease(oldSession); err != nil {
			a.logger.Warn("failed to close rotated socket", "error", err)
		}
	}
	a.disconnectOpenAISocket(oldSocketID, "rotated")
	a.startLeaseLoop(meta)
	a.startIdleLoop(meta)

	payload, err := json.Marshal(map[string]any{
		"reason":                     body.Reason,
		"scheduled_at":               body.ScheduledAt,
		"previous_socket_generation": cmd.ExpectedSocketGeneration,
		"socket_generation":          newGeneration,
	})
	if err != nil {
		return fmt.Errorf("marshal socket rotation event: %w", err)
	}

	if a.history == nil {
		return fmt.Errorf("thread history store is not configured")
	}

	return a.history.AppendEvent(a.ctx, threadstore.EventLogEntry{
		ThreadID:         meta.ID,
		SocketGeneration: meta.SocketGeneration,
		EventType:        "client.socket.rotate",
		PayloadJSON:      string(payload),
		CreatedAt:        now,
	}, fmt.Sprintf("socket-rotate-%d", newGeneration))
}

func (a *threadActor) handleReconcile(cmd threadcmd.Command) error {
	if _, err := cmd.ReconcileBody(); err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	return a.recoverThread(meta, cmd.CmdID, true)
}

func (a *threadActor) recoverThread(meta threadstore.ThreadMeta, cmdID int64, forceReconcile bool) error {
	needsBarrierRecovery := meta.ActiveSpawnGroupID > 0 && meta.ActiveResponseID == "" &&
		(meta.Status == threadstore.ThreadStatusWaitingChildren || meta.Status == threadstore.ThreadStatusRunning || meta.Status == threadstore.ThreadStatusReconciling)

	targetStatus := meta.Status
	switch {
	case needsBarrierRecovery:
		targetStatus = threadstore.ThreadStatusWaitingChildren
	case forceReconcile || meta.Status == threadstore.ThreadStatusRunning || meta.Status == threadstore.ThreadStatusReconciling || meta.ActiveResponseID != "":
		targetStatus = threadstore.ThreadStatusReconciling
	}

	claim, err := a.store.ClaimOwnership(a.ctx, meta.ID, a.workerID, time.Now().UTC().Add(workerLeaseTTL))
	if err != nil {
		return err
	}
	if !claim.Claimed {
		return errOwnershipConflict
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = claim.SocketGeneration
	meta.SocketExpiresAt = time.Now().UTC().Add(socketExpiryTTL)
	meta.Status = targetStatus
	meta.UpdatedAt = time.Now().UTC()

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}
	a.startLeaseLoop(meta)
	a.stopIdleLoop()

	switch meta.Status {
	case threadstore.ThreadStatusWaitingChildren:
		return a.recoverWaitingChildren(meta, cmdID)
	case threadstore.ThreadStatusReconciling:
		return a.reconcileFromCheckpoint(meta, cmdID)
	default:
		if statusSupportsIdleSocket(meta.Status) {
			a.startIdleLoop(meta)
		}
		return nil
	}
}

func (a *threadActor) handleCancel(cmd threadcmd.Command) error {
	if _, err := cmd.CancelBody(); err != nil {
		return err
	}

	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}

	meta.Status = threadstore.ThreadStatusCancelled
	meta.ActiveResponseID = ""
	meta.UpdatedAt = time.Now().UTC()

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}

	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession("cancelled")

	return a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration)
}

func (a *threadActor) handleDisconnectSocket(cmd threadcmd.Command) error {
	meta, err := a.store.LoadThread(a.ctx, cmd.ThreadID)
	if err != nil {
		return err
	}
	if err := validateCommandPreconditions(cmd, meta); err != nil {
		return err
	}
	if !statusSupportsIdleSocket(meta.Status) {
		return fmt.Errorf("%w: cannot disconnect socket for thread in status %s", errCommandPrecond, meta.Status)
	}

	a.logger.Info("disconnecting idle socket",
		appendThreadGraphAttrs([]any{
			"socket_generation", meta.SocketGeneration,
		}, meta)...,
	)

	a.stopIdleLoop()
	a.stopLeaseLoop()
	a.resetSession("idle_disconnect")

	return a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration)
}

func (a *threadActor) recoverWaitingChildren(meta threadstore.ThreadMeta, cmdID int64) error {
	if meta.ActiveSpawnGroupID <= 0 {
		if statusSupportsIdleSocket(meta.Status) {
			a.startIdleLoop(meta)
		}
		return nil
	}

	spawn, err := a.store.LoadSpawnGroup(a.ctx, meta.ActiveSpawnGroupID)
	if err != nil {
		return err
	}

	results, err := a.store.ListSpawnResults(a.ctx, meta.ActiveSpawnGroupID)
	if err != nil {
		return err
	}

	spawn.Completed, spawn.Failed, spawn.Cancelled = summarizeSpawnResults(results)
	if spawn.Completed+spawn.Failed+spawn.Cancelled >= spawn.Expected {
		spawn.Status = threadstore.SpawnGroupStatusClosed
	} else if spawn.Status == "" {
		spawn.Status = threadstore.SpawnGroupStatusWaiting
	}
	spawn.UpdatedAt = time.Now().UTC()

	if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
		return err
	}

	if spawn.Status != threadstore.SpawnGroupStatusClosed {
		meta.Status = threadstore.ThreadStatusWaitingChildren
		if err := a.saveThreadMeta(meta); err != nil {
			return err
		}
		a.logger.Info("child barrier still waiting after recovery",
			appendThreadGraphAttrs([]any{
				"cmd_id", cmdID,
				"spawn_group_id", spawn.ID,
				"completed_children", spawn.Completed,
				"failed_children", spawn.Failed,
				"cancelled_children", spawn.Cancelled,
				"expected_children", spawn.Expected,
			}, meta)...,
		)
		a.startIdleLoop(meta)
		return nil
	}

	outputItem, err := a.aggregateSpawnOutputItemForGroup(spawn, results)
	if err != nil {
		return err
	}

	inputItems, err := normalizeRawItemsAsArray(outputItem)
	if err != nil {
		return err
	}

	if spawn.AggregateSubmittedAt.IsZero() {
		spawn.AggregateSubmittedAt = time.Now().UTC()
		spawn.AggregateCmdID = cmdID
		if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
			return err
		}
	}

	meta.Status = threadstore.ThreadStatusRunning
	meta.UpdatedAt = time.Now().UTC()
	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}

	if err := a.ensureSession(); err != nil {
		return err
	}

	if err := a.appendInputItems(meta.ID, meta.LastResponseID, inputItems); err != nil {
		return err
	}

	a.logger.Info("resuming thread",
		appendThreadGraphAttrs([]any{
			"cmd_id", cmdID,
			"resume_reason", "child_barrier_recovery",
			"input_kind", "function_call_output",
			"spawn_group_id", spawn.ID,
			"completed_children", spawn.Completed,
			"failed_children", spawn.Failed,
			"cancelled_children", spawn.Cancelled,
			"expected_children", spawn.Expected,
		}, meta)...,
	)

	return a.continueWithInputItems(meta, cmdID, inputItems, "child_barrier_recovery")
}

func (a *threadActor) reconcileFromCheckpoint(meta threadstore.ThreadMeta, cmdID int64) error {
	if a.history == nil {
		return fmt.Errorf("thread history store is not configured")
	}

	rawEvent, err := a.history.LoadLatestResponseCreateCheckpoint(a.ctx, meta.ID)
	if err != nil {
		if errors.Is(err, threadhistory.ErrCheckpointNotFound) {
			return a.handleMissingRecoveryCheckpoint(meta)
		}
		return err
	}

	payload, err := extractResponseCreatePayload(rawEvent)
	if err != nil {
		return err
	}
	if err := a.finalizeThreadResponseCreatePayload(meta, payload); err != nil {
		return err
	}

	if err := a.ensureSession(); err != nil {
		return err
	}

	meta.ActiveResponseID = ""
	meta.UpdatedAt = time.Now().UTC()
	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}

	return a.sendAndStream(meta, cmdID, payload, "checkpoint_recovery")
}

func (a *threadActor) handleMissingRecoveryCheckpoint(meta threadstore.ThreadMeta) error {
	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession("missing_recovery_checkpoint")

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
		meta.OwnerWorkerID = 0
		meta.SocketExpiresAt = time.Time{}
	}

	meta.ActiveResponseID = ""
	meta.Status = threadstore.ThreadStatusIncomplete
	meta.UpdatedAt = time.Now().UTC()

	if err := a.saveThreadMeta(meta); err != nil {
		return err
	}

	a.logger.Info("recovery checkpoint missing, leaving thread passive",
		appendThreadGraphAttrs([]any{
			"status", meta.Status,
		}, meta)...,
	)

	return nil
}

func (a *threadActor) ensureSession() error {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()

	if session != nil {
		snapshot := session.Snapshot()
		if snapshot.State == openaiws.SessionStateConnected {
			return nil
		}

		a.logger.Info("existing session not connected, reconnecting",
			appendThreadGraphAttrs([]any{
				"previous_state", snapshot.State,
				"socket_generation", a.currentSocketGeneration(),
				"session_connect_count", snapshot.SocketGeneration,
			}, a.currentMeta())...,
		)
		a.disconnectOpenAISocket(a.currentOpenAISocketID(), "session_not_connected")
	}

	return a.reconnectSession()
}

func (a *threadActor) reconnectSession() error {
	a.logger.Info("connecting openai websocket session",
		appendThreadGraphAttrs([]any{
			"socket_generation", a.currentSocketGeneration(),
		}, a.currentMeta())...,
	)

	newSession, err := a.openFreshSession()
	if err != nil {
		a.logger.Error("failed to connect openai websocket session", "error", err)
		return err
	}

	meta := a.currentMeta()
	leaseUntil := time.Now().UTC().Add(workerLeaseTTL)
	socketID := a.registerOpenAISocket(meta, newSession, leaseUntil)

	a.logger.Info("openai websocket session connected",
		appendThreadGraphAttrs([]any{
			"socket_generation", a.currentSocketGeneration(),
			"session_connect_count", newSession.Snapshot().SocketGeneration,
		}, a.currentMeta())...,
	)

	oldSession, oldSocketID := a.swapSessionState(newSession, socketID)
	if oldSession != nil {
		if err := a.closeAndRelease(oldSession); err != nil {
			a.logger.Warn("failed to close previous socket", "error", err)
		}
	}
	a.disconnectOpenAISocket(oldSocketID, "reconnect")
	return nil
}

// openFreshSession dials a new OpenAI websocket session. The process-wide
// socket budget tracks "actor has any session", not "session opens". A slot
// is reserved only when transitioning from no-session to has-session;
// reconnect/rotate paths that already hold a slot (because a.session != nil)
// inherit it for the new session, so a budget-ceiling situation can still
// recover instead of being wedged at N+1 reservations for a 1-for-1 swap.
//
// On Connect failure we only release if we just reserved (i.e. there was no
// existing session to inherit the slot from).
func (a *threadActor) openFreshSession() (*openaiws.Session, error) {
	a.mu.Lock()
	alreadyHaveSession := a.session != nil
	a.mu.Unlock()

	if !alreadyHaveSession {
		if a.budgetReserve != nil {
			if err := a.budgetReserve(); err != nil {
				return nil, fmt.Errorf("%w: %v", errSocketBudgetExhausted, err)
			}
		}
	}

	session := a.sessionFactory()
	if err := session.Connect(a.ctx); err != nil {
		if !alreadyHaveSession && a.budgetRelease != nil {
			a.budgetRelease()
		}
		return nil, err
	}
	return session, nil
}

// closeAndRelease closes a session and conditionally releases the budget
// slot. The slot is only released if a.session is now nil after the close —
// in the replacement path (rotate/reconnect) the new session has already
// been swapped in, so the slot is still held for that new session and we
// must not release it here.
//
// Use this instead of calling session.Close() directly so the process-wide
// socket budget stays accurate across all close paths.
func (a *threadActor) closeAndRelease(session *openaiws.Session) error {
	if session == nil {
		return nil
	}
	err := session.Close()

	a.mu.Lock()
	hasReplacement := a.session != nil
	a.mu.Unlock()

	if !hasReplacement && a.budgetRelease != nil {
		a.budgetRelease()
	}
	return err
}

func (a *threadActor) swapSessionState(next *openaiws.Session, nextSocketID string) (*openaiws.Session, string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	prev := a.session
	prevSocketID := a.openAISocketID
	a.session = next
	a.openAISocketID = nextSocketID
	return prev, prevSocketID
}

func (a *threadActor) startLeaseLoop(meta threadstore.ThreadMeta) {
	a.mu.Lock()
	if a.leaseCancel != nil {
		a.leaseCancel()
	}

	leaseCtx, cancel := context.WithCancel(a.ctx)
	a.leaseCancel = cancel
	a.mu.Unlock()

	go a.runLeaseLoop(leaseCtx, meta)
}

func (a *threadActor) stopLeaseLoop() {
	a.mu.Lock()
	cancel := a.leaseCancel
	a.leaseCancel = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (a *threadActor) runLeaseLoop(ctx context.Context, meta threadstore.ThreadMeta) {
	interval := workerLeaseTTL / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			leaseUntil := time.Now().UTC().Add(workerLeaseTTL)
			renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			renewed, err := a.store.RenewOwnership(renewCtx, meta.ID, a.workerID, meta.SocketGeneration, leaseUntil)
			cancel()
			if err != nil {
				a.logger.Warn("failed to renew thread lease", "socket_generation", meta.SocketGeneration, "error", err)
				continue
			}
			if !renewed {
				a.logger.Warn("lost thread lease", "socket_generation", meta.SocketGeneration)
				a.handleLeaseLoss(meta.SocketGeneration)
				return
			}

			a.mu.Lock()
			session := a.session
			a.mu.Unlock()
			a.touchOpenAISocket(session, threadstore.OpenAISocketTouch{
				LastHeartbeatAt:    time.Now().UTC(),
				HeartbeatExpiresAt: leaseUntil,
			})
		}
	}
}

func (a *threadActor) handleLeaseLoss(socketGeneration uint64) {
	a.mu.Lock()
	if a.meta.SocketGeneration != socketGeneration {
		a.mu.Unlock()
		return
	}

	if a.leaseCancel != nil {
		a.leaseCancel()
		a.leaseCancel = nil
	}
	if a.idleCancel != nil {
		a.idleCancel()
		a.idleCancel = nil
	}

	session := a.session
	socketID := a.openAISocketID
	a.session = nil
	a.openAISocketID = ""
	idleDone := a.idleDone
	a.mu.Unlock()

	if idleDone != nil {
		<-idleDone
	}

	if session != nil {
		_ = a.closeAndRelease(session)
	}
	a.disconnectOpenAISocket(socketID, "lease_lost")

	a.cancel()
}

func (a *threadActor) sendAndStream(meta threadstore.ThreadMeta, cmdID int64, payload map[string]any, trigger string) error {
	turn := 0
	for {
		if err := a.submitResponseCreate(meta, cmdID, turn, payload, trigger); err != nil {
			return err
		}

		pending, err := a.streamUntilTerminal(meta)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}

		// Sync tool outputs pending. Re-load meta (streamUntilTerminal persisted
		// LastResponseID etc.), execute the tools, and loop with a follow-up
		// response.create carrying their outputs as input.
		refreshed, err := a.store.LoadThread(a.ctx, meta.ID)
		if err != nil {
			return fmt.Errorf("reload thread for page-read followup: %w", err)
		}
		meta = refreshed

		inputItems, err := a.executePendingPageReads(meta, pending)
		if err != nil {
			return err
		}
		if err := a.appendInputItems(meta.ID, meta.LastResponseID, inputItems); err != nil {
			return err
		}

		payload, err = a.buildThreadResponseCreatePayload(meta, map[string]any{
			"model":                meta.Model,
			"instructions":         meta.Instructions,
			"input":                inputItems,
			"previous_response_id": meta.LastResponseID,
			"store":                true,
		})
		if err != nil {
			return err
		}
		trigger = "page_read_followup"
		turn++
	}
}

// submitResponseCreateEventID derives a unique event identifier per submit
// within a single sendAndStream invocation. The first turn reuses the raw
// command id for backwards compatibility with existing telemetry; follow-up
// turns (sync-tool resume) append a turn suffix so JetStream dedup on
// Nats-Msg-Id doesn't silently drop the follow-up checkpoint / event-log
// entries in threadhistory (see threadhistory/store.go — CheckpointMsgID and
// EventMsgID both hash the passed-in id).
func submitResponseCreateEventID(cmdID int64, turn int) string {
	base := formatCommandIDLocal(cmdID)
	if turn == 0 {
		return base
	}
	return base + "-turn-" + strconv.Itoa(turn)
}

func (a *threadActor) submitResponseCreate(meta threadstore.ThreadMeta, cmdID int64, turn int, payload map[string]any, trigger string) error {
	eventID := submitResponseCreateEventID(cmdID, turn)

	logPayload, err := marshalResponseCreatePayload(payload)
	if err != nil {
		return fmt.Errorf("marshal response.create payload for log: %w", err)
	}

	logEvent, err := openaiws.NewResponseCreateEvent(eventID, logPayload)
	if err != nil {
		return err
	}

	rawEvent, err := logEvent.Bytes()
	if err != nil {
		return err
	}

	sendPayload, err := a.materializePreparedInputPayload(payload)
	if err != nil {
		return err
	}

	wirePayload, stats, err := a.lowerResponseCreatePayload(sendPayload)
	if err != nil {
		return err
	}

	wirePayloadJSON, err := marshalResponseCreatePayload(wirePayload)
	if err != nil {
		return fmt.Errorf("marshal response.create payload for wire: %w", err)
	}

	event, err := openaiws.NewResponseCreateEvent(eventID, wirePayloadJSON)
	if err != nil {
		return err
	}

	a.logger.Info("sending response.create to openai",
		appendThreadGraphAttrs([]any{
			"trigger", trigger,
			"input_kind", responseCreateInputKind(payload),
			"has_previous_response_id", strings.TrimSpace(stringValue(payload["previous_response_id"])) != "",
			"model", meta.Model,
			"reasoning_effort", responseCreateReasoningEffort(wirePayload),
			"socket_generation", meta.SocketGeneration,
			"input_items_count", stats.InputItemsCount,
			"lowered_image_inputs", stats.LoweredImageInputs,
			"lowered_blob_refs", stats.LoweredBlobRefs,
		}, meta)...,
	)

	if err := a.sendResponseCreate(event); err != nil {
		return err
	}

	if a.history == nil {
		return fmt.Errorf("thread history store is not configured")
	}
	if err := a.history.SaveResponseCreateCheckpoint(a.ctx, meta.ID, eventID, rawEvent); err != nil {
		return err
	}

	if err := a.history.AppendEvent(a.ctx, threadstore.EventLogEntry{
		ThreadID:         meta.ID,
		SocketGeneration: meta.SocketGeneration,
		EventType:        "client.response.create",
		PayloadJSON:      string(rawEvent),
		CreatedAt:        time.Now().UTC(),
	}, "client-response-create-"+eventID); err != nil {
		return err
	}

	if a.publishEvent != nil {
		if err := a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, "client-response-create", eventrelay.EventTypeClientResponse, injectThreadFields(meta, rawEvent)); err != nil {
			return err
		}
	}

	return nil
}

func (a *threadActor) lowerResponseCreatePayload(payload map[string]any) (map[string]any, payloadLoweringStats, error) {
	return lowerResponseCreatePayloadWithBlob(a.ctx, a.blob, payload)
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneAny(nested)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneAny(nested)
		}
		return cloned
	default:
		return value
	}
}

func (a *threadActor) lowerInputValue(value any, stats *payloadLoweringStats) (any, error) {
	return lowerInputValueWithBlob(a.ctx, a.blob, value, stats)
}

func (a *threadActor) lowerInputItem(item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	return lowerInputItemWithBlob(a.ctx, a.blob, item, stats)
}

func (a *threadActor) lowerMessageContentItem(item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	return lowerMessageContentItemWithBlob(a.ctx, a.blob, item, stats)
}

func (a *threadActor) buildInputImageFromBlobRef(item map[string]any, ref string) (map[string]any, error) {
	return buildInputImageFromBlobRefWithBlob(a.ctx, a.blob, item, ref)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func hasLogAttrKey(attrs []any, key string) bool {
	for index := 0; index+1 < len(attrs); index += 2 {
		attrKey, ok := attrs[index].(string)
		if ok && attrKey == key {
			return true
		}
	}
	return false
}

func appendThreadGraphAttrs(attrs []any, meta threadstore.ThreadMeta) []any {
	if rootThreadID := defaultRootThreadID(meta); rootThreadID > 0 && !hasLogAttrKey(attrs, "root_thread_id") {
		attrs = append(attrs, "root_thread_id", rootThreadID)
	}
	if parentThreadID := meta.ParentThreadID; parentThreadID > 0 && !hasLogAttrKey(attrs, "parent_thread_id") {
		attrs = append(attrs, "parent_thread_id", parentThreadID)
	}
	if parentCallID := strings.TrimSpace(meta.ParentCallID); parentCallID != "" && !hasLogAttrKey(attrs, "parent_call_id") {
		attrs = append(attrs, "parent_call_id", parentCallID)
	}
	if !hasLogAttrKey(attrs, "depth") {
		attrs = append(attrs, "depth", meta.Depth)
	}
	if spawnGroupID := meta.ActiveSpawnGroupID; spawnGroupID > 0 && !hasLogAttrKey(attrs, "spawn_group_id") {
		attrs = append(attrs, "spawn_group_id", spawnGroupID)
	}
	return attrs
}

func responseCreateInputKind(payload map[string]any) string {
	if strings.TrimSpace(stringValue(payload["prepared_input_ref"])) != "" {
		return "prepared_input"
	}

	items := decodeResponseCreateInputItems(payload["input"])
	if len(items) == 0 {
		return "none"
	}

	itemType := strings.TrimSpace(stringValue(items[0]["type"]))
	switch itemType {
	case "message":
		if strings.TrimSpace(stringValue(items[0]["role"])) == "user" {
			return "user_message"
		}
		return "message"
	case "":
		return "input_items"
	default:
		return itemType
	}
}

func decodeResponseCreateInputItems(value any) []map[string]any {
	switch typed := value.(type) {
	case json.RawMessage:
		var items []map[string]any
		if err := json.Unmarshal(typed, &items); err == nil {
			return items
		}
	case []map[string]any:
		return typed
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			decoded, ok := item.(map[string]any)
			if !ok {
				return nil
			}
			items = append(items, decoded)
		}
		return items
	}
	return nil
}

func (a *threadActor) continueWithInputItems(meta threadstore.ThreadMeta, cmdID int64, inputItems json.RawMessage, trigger string) error {
	return a.continueWithPreparedInput(meta, cmdID, inputItems, "", trigger)
}

func (a *threadActor) continueWithPreparedInput(meta threadstore.ThreadMeta, cmdID int64, inputItems json.RawMessage, preparedInputRef string, trigger string) error {
	payload, err := a.buildThreadResponseCreatePayload(meta, map[string]any{
		"model":                meta.Model,
		"instructions":         meta.Instructions,
		"input":                inputItems,
		"prepared_input_ref":   preparedInputRef,
		"previous_response_id": meta.LastResponseID,
		"store":                true,
	})
	if err != nil {
		return err
	}

	return a.sendAndStream(meta, cmdID, payload, trigger)
}

func (a *threadActor) applyDocumentRuntimeContext(meta threadstore.ThreadMeta, payload map[string]any) error {
	if a.threadDocs == nil {
		return nil
	}

	threadID := meta.ID
	documents, err := a.threadDocs.ListDocuments(a.ctx, threadID, 200)
	if err != nil {
		return fmt.Errorf("list attached documents for runtime context: %w", err)
	}
	var collections []threadcollectionstore.AttachedCollection
	if a.threadCollections != nil {
		collections, err = a.threadCollections.ListAttached(a.ctx, threadID, 200)
		if err != nil {
			return fmt.Errorf("list attached collections for runtime context: %w", err)
		}
	}
	if len(documents) == 0 && len(collections) == 0 {
		return nil
	}
	if a.docRuntime == nil {
		return a.applyLocalDocumentRuntimeContext(meta, payload, documents, collections)
	}

	rawTools, err := marshalRuntimeContextTools(payload["tools"])
	if err != nil {
		return err
	}

	requestID := fmt.Sprintf("docctx_%d_%d", threadID, time.Now().UTC().UnixNano())
	resp, err := a.docRuntime.RuntimeContext(a.ctx, doccmd.RuntimeContextRequest{
		RequestID:      requestID,
		ThreadID:       threadID,
		ParentThreadID: meta.ParentThreadID,
		Instructions:   stringValue(payload["instructions"]),
		Tools:          rawTools,
	})
	if err != nil {
		return a.fallbackDocumentRuntimeContext(meta, payload, documents, collections, fmt.Errorf("load document runtime context: %w", err))
	}
	if resp.RequestID != requestID {
		return a.fallbackDocumentRuntimeContext(meta, payload, documents, collections, fmt.Errorf("document runtime context request id mismatch: got %q want %q", resp.RequestID, requestID))
	}
	if strings.TrimSpace(resp.Status) != doccmd.PrepareStatusOK {
		if strings.TrimSpace(resp.Error) == "" {
			resp.Error = fmt.Sprintf("runtime context returned status %q", resp.Status)
		}
		return a.fallbackDocumentRuntimeContext(meta, payload, documents, collections, fmt.Errorf("document runtime context: %s", resp.Error))
	}

	if strings.TrimSpace(resp.Instructions) == "" {
		delete(payload, "instructions")
	} else {
		payload["instructions"] = resp.Instructions
	}

	if len(resp.Tools) == 0 {
		delete(payload, "tools")
		return nil
	}

	decoded, err := decodeToolsParam(resp.Tools)
	if err != nil {
		return a.fallbackDocumentRuntimeContext(meta, payload, documents, collections, fmt.Errorf("decode document runtime tools: %w", err))
	}
	payload["tools"] = decoded
	return nil
}

func (a *threadActor) fallbackDocumentRuntimeContext(meta threadstore.ThreadMeta, payload map[string]any, documents []docstore.Document, collections []threadcollectionstore.AttachedCollection, cause error) error {
	if a.logger != nil {
		a.logger.Warn("document runtime context unavailable, falling back to local augmentation",
			"document_count", len(documents),
			"collection_count", len(collections),
			"error", cause,
		)
	}
	return a.applyLocalDocumentRuntimeContext(meta, payload, documents, collections)
}

func (a *threadActor) applyLocalDocumentRuntimeContext(meta threadstore.ThreadMeta, payload map[string]any, documents []docstore.Document, collections []threadcollectionstore.AttachedCollection) error {
	instructions := docprompt.AppendAvailableDocumentsBlock(stringValue(payload["instructions"]), documents, collections)
	if strings.TrimSpace(instructions) == "" {
		delete(payload, "instructions")
	} else {
		payload["instructions"] = instructions
	}

	rawTools, err := marshalRuntimeContextTools(payload["tools"])
	if err != nil {
		return err
	}
	rawTools, err = appendQueryDocumentToolLocal(rawTools)
	if err != nil {
		return err
	}
	if meta.ParentThreadID == 0 {
		rawTools, err = appendReadDocumentPageToolLocal(rawTools)
		if err != nil {
			return err
		}
	}
	if len(rawTools) == 0 {
		delete(payload, "tools")
		return nil
	}

	decoded, err := decodeToolsParam(rawTools)
	if err != nil {
		return fmt.Errorf("decode local document runtime tools: %w", err)
	}
	payload["tools"] = decoded
	return nil
}

func marshalRuntimeContextTools(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime context tools: %w", err)
	}
	return raw, nil
}

func appendQueryDocumentToolLocal(raw json.RawMessage) (json.RawMessage, error) {
	return appendToolIfMissingLocal(raw, doccmd.ToolNameQueryDocument, doccmd.QueryDocumentToolDefinition)
}

func appendReadDocumentPageToolLocal(raw json.RawMessage) (json.RawMessage, error) {
	return appendToolIfMissingLocal(raw, doccmd.ToolNameReadDocumentPage, doccmd.ReadDocumentPageToolDefinition)
}

func appendToolIfMissingLocal(raw json.RawMessage, name string, definition func() map[string]any) (json.RawMessage, error) {
	var tools []map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode local document runtime tools: %w", err)
		}
	}

	for _, tool := range tools {
		existing, _ := tool["name"].(string)
		if existing == name {
			return raw, nil
		}
	}

	tools = append(tools, definition())
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("marshal local document runtime tools: %w", err)
	}
	return encoded, nil
}

type docQueryRequest struct {
	DocumentID int64  `json:"document_id"`
	Task       string `json:"task"`
}

type docQueryCall struct {
	CallID  string
	Request docQueryRequest
}

type docQueryRoundCall struct {
	CallID     string `json:"call_id"`
	DocumentID int64  `json:"document_id,omitempty"`
	Task       string `json:"task,omitempty"`
}

type docQueryDocWork struct {
	DocumentID int64
	Calls      []docQueryRoundCall
}

func decodeDocQueryRequest(arguments string) (docQueryRequest, error) {
	var req docQueryRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return docQueryRequest{}, fmt.Errorf("decode %s arguments: %w", doccmd.ToolNameQueryDocument, err)
	}
	if req.DocumentID <= 0 {
		return docQueryRequest{}, fmt.Errorf("%s requires a positive document_id", doccmd.ToolNameQueryDocument)
	}
	if strings.TrimSpace(req.Task) == "" {
		return docQueryRequest{}, fmt.Errorf("%s requires a non-empty task", doccmd.ToolNameQueryDocument)
	}
	return req, nil
}

type pageReadRequest struct {
	DocumentID int64 `json:"document_id"`
	PageNumber int   `json:"page_number"`
}

type pageReadCall struct {
	CallID  string
	Request pageReadRequest
}

func decodePageReadRequest(arguments string) (pageReadRequest, error) {
	var req pageReadRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return pageReadRequest{}, fmt.Errorf("decode %s arguments: %w", doccmd.ToolNameReadDocumentPage, err)
	}
	if req.DocumentID <= 0 {
		return pageReadRequest{}, fmt.Errorf("%s requires a positive document_id", doccmd.ToolNameReadDocumentPage)
	}
	if req.PageNumber <= 0 {
		return pageReadRequest{}, fmt.Errorf("%s requires a positive page_number", doccmd.ToolNameReadDocumentPage)
	}
	return req, nil
}

func (a *threadActor) streamUntilTerminal(meta threadstore.ThreadMeta) ([]pageReadCall, error) {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return nil, openaiws.ErrNotConnected
	}

	a.logger.Info("streaming response events from openai",
		appendThreadGraphAttrs([]any{
			"socket_generation", meta.SocketGeneration,
		}, meta)...,
	)

	waitingTool := false
	var pendingSpawn *spawnRequest
	var pendingSpawnCallID string
	var pendingDocQueries []docQueryCall
	var pendingPageReads []pageReadCall
	var pendingCitationCalls []citationLocatorCall
	prevStatus := meta.Status
	eventCount := 0
	deltaLogs := map[openaiws.EventType]*deltaLogState{}
	suppressLiveDeltas := meta.ParentThreadID > 0

	for {
		event, err := session.Receive(a.ctx)
		if err != nil {
			socketID := a.currentOpenAISocketID()
			a.disconnectOpenAISocket(socketID, "stream_receive_error")
			a.logger.Error("stream receive error",
				"events_received", eventCount,
				"error", err,
			)
			return nil, err
		}
		eventCount++
		responseID := event.ResolvedResponseID()

		if event.Type.IsReasoningDelta() || event.Type.IsToolCallDelta() {
			continue
		}

		isDelta := event.Type.IsDelta()

		if isDelta {
			state := deltaLogs[event.Type]
			if state == nil {
				state = &deltaLogState{}
				deltaLogs[event.Type] = state
			}
			if !state.firstLogged {
				raw := string(event.Raw)
				state.firstRaw = raw
				state.lastRaw = raw
				state.firstLogged = true
				a.logOpenAIEvent(meta, responseID, "event_type", event.Type)
			} else {
				state.lastRaw = string(event.Raw)
				state.suppressedAny = true
			}
		} else if event.Type == openaiws.EventTypeResponseOutputItemAdded {
			a.flushDeltaLogForDoneEvent(deltaLogs, meta, responseID, event.Type)
			a.logOutputItemEvent(meta, responseID, event)
		} else if event.Type == openaiws.EventTypeResponseOutputItemDone {
			a.flushDeltaLogForDoneEvent(deltaLogs, meta, responseID, event.Type)
			a.logOutputItemEvent(meta, responseID, event)
		} else {
			a.flushDeltaLogForDoneEvent(deltaLogs, meta, responseID, event.Type)
			a.logOpenAIEvent(meta, responseID, "event_type", event.Type)
		}
		if !isDelta {
			a.touchOpenAISocket(session, threadstore.OpenAISocketTouch{})
		}
		if !isDelta {
			if a.history == nil {
				return nil, fmt.Errorf("thread history store is not configured")
			}
			if err := a.history.AppendEvent(a.ctx, threadstore.EventLogEntry{
				ThreadID:         meta.ID,
				SocketGeneration: meta.SocketGeneration,
				EventType:        string(event.Type),
				ResponseID:       responseID,
				PayloadJSON:      string(event.Raw),
				CreatedAt:        time.Now().UTC(),
			}, ""); err != nil {
				return nil, err
			}
		}

		if a.publishEvent != nil && (!suppressLiveDeltas || !isDelta) {
			if err := a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, fmt.Sprintf("event-%d", eventCount), string(event.Type), injectThreadFields(meta, event.Raw)); err != nil {
				return nil, err
			}
		}

		if responsePayload := event.ResponsePayload(); len(responsePayload) > 0 {
			if err := a.store.SaveResponseRaw(a.ctx, meta.ID, responseID, responsePayload); err != nil {
				return nil, err
			}
		}

		switch event.Type {
		case openaiws.EventTypeResponseCreated, openaiws.EventTypeResponseInProgress:
			if responseID != "" {
				meta.ActiveResponseID = responseID
			}
			meta.Status = threadstore.ThreadStatusRunning
		case openaiws.EventTypeResponseOutputItemDone:
			itemRaw := event.Field("item")
			if len(itemRaw) > 0 {
				itemType := itemTypeFromRaw(itemRaw)
				if itemType == "function_call" {
					call, err := parseFunctionCallItem(itemRaw)
					if err == nil && call.Name == "spawn_threads" {
						req, err := decodeSpawnRequest(call.Arguments, meta)
						if err != nil {
							return nil, err
						}
						pendingSpawn = &req
						pendingSpawnCallID = call.CallID
					} else if err == nil && call.Name == doccmd.ToolNameQueryDocument {
						if len(pendingDocQueries) >= maxChildrenPerSpawn {
							a.logger.Warn("query_document per-turn cap exceeded, halting spawn",
								"cap", maxChildrenPerSpawn,
								"call_id", call.CallID,
							)
							waitingTool = true
						} else {
							req, err := decodeDocQueryRequest(call.Arguments)
							if err != nil {
								a.logger.Warn("invalid document query arguments, falling back to waiting_tool",
									"call_id", call.CallID,
									"error", err,
								)
								waitingTool = true
							} else {
								pendingDocQueries = append(pendingDocQueries, docQueryCall{
									CallID:  call.CallID,
									Request: req,
								})
							}
						}
					} else if err == nil && call.Name == doccmd.ToolNameReadDocumentPage {
						req, err := decodePageReadRequest(call.Arguments)
						if err != nil {
							a.logger.Warn("invalid read_document_page arguments, falling back to waiting_tool",
								"call_id", call.CallID,
								"error", err,
							)
							waitingTool = true
						} else {
							pendingPageReads = append(pendingPageReads, pageReadCall{
								CallID:  call.CallID,
								Request: req,
							})
						}
					} else if err == nil && call.Name == doccmd.ToolNameStoreCitation {
						if len(pendingCitationCalls) >= maxCitationsPerTurn {
							a.logger.Warn("store_citation per-turn cap exceeded, halting spawn",
								"cap", maxCitationsPerTurn,
								"call_id", call.CallID,
							)
							waitingTool = true
						} else {
							req, err := decodeCitationLocatorRequest(call.Arguments)
							if err != nil {
								a.logger.Warn("invalid store_citation arguments, falling back to waiting_tool",
									"call_id", call.CallID,
									"error", err,
								)
								waitingTool = true
							} else {
								pendingCitationCalls = append(pendingCitationCalls, citationLocatorCall{
									CallID:  call.CallID,
									Request: req,
								})
							}
						}
					} else {
						waitingTool = true
					}
				}
				a.logger.Info("output item received",
					appendThreadGraphAttrs(append([]any{
						"item_type", itemType,
						"response_id", responseID,
					}, outputItemSemanticAttrs(itemRaw)...), meta)...,
				)
				if _, err := a.appendItem(threadstore.ItemLogEntry{
					ThreadID:    meta.ID,
					ResponseID:  responseID,
					ItemType:    itemType,
					Direction:   "output",
					PayloadJSON: string(itemRaw),
					CreatedAt:   time.Now().UTC(),
				}); err != nil {
					return nil, err
				}
			}
		case openaiws.EventTypeResponseCompleted:
			if responseID != "" {
				meta.LastResponseID = responseID
			}
			meta.ActiveResponseID = ""
			// Refuse turns that mix tool kinds the dispatch can't fan out
			// in parallel. Spawning a locator/query group and returning
			// sync-tool outputs in the same barrier cycle would leave
			// whichever kind lost the priority chain with dangling
			// function_calls (no matching function_call_output in the
			// follow-up turn, which OpenAI rejects). Fall back to
			// waiting_tool so the mix surfaces as an explicit stuck
			// state instead of a silent drop. Matches how malformed
			// tool args already behave.
			pendingKinds := 0
			if pendingSpawn != nil {
				pendingKinds++
			}
			if len(pendingDocQueries) > 0 {
				pendingKinds++
			}
			if len(pendingCitationCalls) > 0 {
				pendingKinds++
			}
			if len(pendingPageReads) > 0 {
				pendingKinds++
			}
			if pendingKinds > 1 {
				a.logger.Warn("mixed tool kinds in one response, refusing dispatch",
					appendThreadGraphAttrs([]any{
						"pending_spawn", pendingSpawn != nil,
						"pending_doc_queries", len(pendingDocQueries),
						"pending_citation_calls", len(pendingCitationCalls),
						"pending_page_reads", len(pendingPageReads),
					}, meta)...,
				)
				waitingTool = true
			}
			if pendingSpawn != nil && !waitingTool {
				spawnGroupID, err := a.startSpawnGroup(meta, pendingSpawnCallID, *pendingSpawn)
				if err != nil {
					return nil, err
				}
				meta.ActiveSpawnGroupID = spawnGroupID
				meta.Status = threadstore.ThreadStatusWaitingChildren
			} else if len(pendingDocQueries) > 0 && !waitingTool {
				spawnGroupID, err := a.startDocumentQueryGroup(meta, pendingDocQueries)
				if err != nil {
					return nil, err
				}
				meta.ActiveSpawnGroupID = spawnGroupID
				meta.Status = threadstore.ThreadStatusWaitingChildren
			} else if len(pendingCitationCalls) > 0 && !waitingTool {
				spawnGroupID, err := a.startCitationLocatorGroup(meta, pendingCitationCalls)
				if err != nil {
					return nil, err
				}
				meta.ActiveSpawnGroupID = spawnGroupID
				meta.Status = threadstore.ThreadStatusWaitingChildren
			} else if len(pendingPageReads) > 0 && !waitingTool {
				// Sync tool: stream terminates here but the thread is not done.
				// Caller (sendAndStream loop) will execute the page reads and
				// fire a follow-up response.create with their outputs as input.
				meta.Status = threadstore.ThreadStatusRunning
			} else if waitingTool {
				meta.Status = threadstore.ThreadStatusWaitingTool
			} else {
				meta.Status = successfulResponseThreadStatus(meta)
			}
		case openaiws.EventTypeResponseFailed:
			if responseID != "" {
				meta.LastResponseID = responseID
			}
			meta.ActiveResponseID = ""
			meta.Status = threadstore.ThreadStatusFailed
		case openaiws.EventTypeResponseIncomplete:
			if responseID != "" {
				meta.LastResponseID = responseID
			}
			meta.ActiveResponseID = ""
			meta.Status = threadstore.ThreadStatusIncomplete
		case openaiws.EventTypeError:
			meta.ActiveResponseID = ""
			meta.Status = threadstore.ThreadStatusFailed
		}

		if meta.Status != prevStatus {
			a.logger.Info("thread status changed",
				appendThreadGraphAttrs([]any{
					"from", prevStatus,
					"to", meta.Status,
					"event_type", event.Type,
					"response_id", responseID,
				}, meta)...,
			)
			prevStatus = meta.Status
		}

		meta.UpdatedAt = time.Now().UTC()
		if err := a.saveThreadMeta(meta); err != nil {
			return nil, err
		}

		if event.Type == openaiws.EventTypeError {
			a.resetSession("remote_error_event")
			if event.Error != nil && event.Error.Message != "" {
				return nil, fmt.Errorf("%w: %s", errRemotePermanent, event.Error.Message)
			}
			return nil, fmt.Errorf("%w: openai error event received", errRemotePermanent)
		}

		if event.Type.IsTerminal() {
			a.flushAllDeltaLogs(deltaLogs, meta, responseID)
			a.logger.Info("stream completed",
				appendThreadGraphAttrs([]any{
					"final_status", meta.Status,
					"last_response_id", meta.LastResponseID,
					"events_received", eventCount,
					"pending_page_reads", len(pendingPageReads),
				}, meta)...,
			)
			// Continuing with sync tool outputs — skip terminal cleanup and let
			// sendAndStream fire the follow-up response.create. Gated on
			// response.completed: if the response failed or incomplete'd mid-
			// stream we must not execute the tool calls it emitted, because
			// meta.Status is already Failed/Incomplete and we'd be firing a
			// new response.create on a terminated thread.
			//
			// Also gated on no pendingCitationCalls: the response.completed
			// dispatch above gives citation locator spawn priority over page
			// reads, transitioning the thread to WaitingChildren. If we
			// returned pendingPageReads here anyway, sendAndStream would fire
			// a follow-up response.create on a thread already waiting on the
			// citation barrier — racing barrier close against the page-read
			// continuation.
			if event.Type == openaiws.EventTypeResponseCompleted &&
				len(pendingPageReads) > 0 &&
				pendingSpawn == nil &&
				len(pendingDocQueries) == 0 &&
				len(pendingCitationCalls) == 0 &&
				!waitingTool {
				return pendingPageReads, nil
			}
			publishMeta := meta
			childResultStatus, ok := childInvocationResultStatus(meta.Status, childInvocationResultStatusForTerminalEvent(event.Type))
			if shouldReleaseTerminalChildResources(meta) {
				if err := a.releaseTerminalChildResources(&meta); err != nil {
					return nil, err
				}
			}
			if err := a.clearCompletedInvocationState(&meta); err != nil {
				return nil, err
			}
			if shouldPublishChildInvocationResult(publishMeta, childResultStatus, ok) {
				if err := a.publishChildInvocationResult(publishMeta, childResultStatus); err != nil {
					return nil, err
				}
			}
			// One-shot children (e.g. the evidence locator) are explicitly single-
			// response: we disconnect the socket right after reporting instead of
			// holding it warm for a follow-up that will never come. The parent
			// barrier already has the result; the child has nothing more to do.
			if publishMeta.OneShot {
				socketID := a.currentOpenAISocketID()
				a.disconnectOpenAISocket(socketID, "one_shot_complete")
				a.resetSession("one_shot_complete")
			} else if statusSupportsIdleSocket(meta.Status) {
				a.startIdleLoop(meta)
			}
			return nil, nil
		}
	}
}

func (a *threadActor) flushDeltaLogForDoneEvent(deltaLogs map[openaiws.EventType]*deltaLogState, meta threadstore.ThreadMeta, responseID string, eventType openaiws.EventType) {
	if !strings.HasSuffix(string(eventType), ".done") {
		return
	}

	deltaType := openaiws.EventType(strings.TrimSuffix(string(eventType), ".done") + ".delta")
	a.flushDeltaLog(deltaLogs, meta, responseID, deltaType)
}

func (a *threadActor) flushAllDeltaLogs(deltaLogs map[openaiws.EventType]*deltaLogState, meta threadstore.ThreadMeta, responseID string) {
	for eventType := range deltaLogs {
		a.flushDeltaLog(deltaLogs, meta, responseID, eventType)
	}
}

func (a *threadActor) flushDeltaLog(deltaLogs map[openaiws.EventType]*deltaLogState, meta threadstore.ThreadMeta, responseID string, eventType openaiws.EventType) {
	state := deltaLogs[eventType]
	if state == nil {
		return
	}
	delete(deltaLogs, eventType)

	if !state.suppressedAny || state.lastRaw == "" || state.lastRaw == state.firstRaw {
		return
	}

	a.logOpenAIEvent(meta, responseID, "event_type", eventType)
}

func (a *threadActor) startIdleLoop(meta threadstore.ThreadMeta) {
	if !statusSupportsIdleSocket(meta.Status) {
		return
	}

	// coder/websocket Ping requires an active reader to consume the pong.
	// We do not have a background reader while a thread is idle, so a heartbeat
	// loop would create false liveness failures and force unnecessary reconnects.
}

func (a *threadActor) stopIdleLoop() {
	a.mu.Lock()
	cancel := a.idleCancel
	done := a.idleDone
	a.idleCancel = nil
	a.idleDone = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (a *threadActor) runIdleLoop(ctx context.Context, done chan struct{}, meta threadstore.ThreadMeta, session *openaiws.Session) {
	defer close(done)

	interval := a.cfg.PingInterval
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.shouldMaintainIdleSocket(meta.SocketGeneration) {
				return
			}

			if err := a.idleHeartbeat(ctx, meta, session); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				a.logger.Warn("idle socket heartbeat failed",
					"socket_generation", meta.SocketGeneration,
					"error", err,
				)
				return
			}
		}
	}
}

func (a *threadActor) shouldMaintainIdleSocket(socketGeneration uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session == nil {
		return false
	}

	if a.meta.SocketGeneration != socketGeneration {
		return false
	}

	return statusSupportsIdleSocket(a.meta.Status)
}

func (a *threadActor) idleHeartbeat(ctx context.Context, meta threadstore.ThreadMeta, session *openaiws.Session) error {
	_ = ctx
	_ = meta
	_ = session
	return nil
}

func (a *threadActor) sendResponseCreate(event openaiws.ClientEvent) error {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()

	if session == nil {
		return openaiws.ErrNotConnected
	}

	if err := session.Send(a.ctx, event); err == nil {
		a.touchOpenAISocket(session, threadstore.OpenAISocketTouch{})
		return nil
	} else {
		a.logger.Warn("response.create send failed, reconnecting",
			"error", err,
		)
	}

	if err := a.reconnectSession(); err != nil {
		return err
	}

	a.mu.Lock()
	session = a.session
	a.mu.Unlock()
	if session == nil {
		return openaiws.ErrNotConnected
	}

	return session.Send(a.ctx, event)
}

func (a *threadActor) appendInputItems(threadID int64, responseID string, itemsRaw json.RawMessage) error {
	items, err := decodeJSONArray(itemsRaw)
	if err != nil {
		return err
	}

	for _, item := range items {
		if _, err := a.appendItem(threadstore.ItemLogEntry{
			ThreadID:    threadID,
			ResponseID:  responseID,
			ItemType:    itemTypeFromRaw(item),
			Direction:   "input",
			PayloadJSON: string(item),
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			return err
		}
	}

	return nil
}

func (a *threadActor) materializePreparedInputPayload(payload map[string]any) (map[string]any, error) {
	return materializePreparedInputPayloadWithBlob(a.ctx, a.blob, payload)
}

func (a *threadActor) loadPreparedInputValue(ref string) (any, error) {
	return loadPreparedInputValueFromBlob(a.ctx, a.blob, ref)
}

// injectThreadFields prepends thread_id, root_thread_id, and parent_thread_id into the JSON object payload.
func injectThreadFields(meta threadstore.ThreadMeta, raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return raw
	}
	prefix := fmt.Sprintf(`"thread_id":%d,"root_thread_id":%d,"parent_thread_id":%d`,
		meta.ID, meta.RootThreadID, meta.ParentThreadID)
	inner := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
	out := make([]byte, 0, len(trimmed)+len(prefix)+2)
	out = append(out, '{')
	out = append(out, prefix...)
	if len(inner) > 0 {
		out = append(out, ',')
		out = append(out, inner...)
	}
	out = append(out, '}')
	return out
}

func (a *threadActor) appendItem(entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error) {
	return a.store.AppendItem(a.ctx, entry)
}

func (a *threadActor) saveThreadMeta(meta threadstore.ThreadMeta) error {
	if err := a.store.SaveThread(a.ctx, meta); err != nil {
		return err
	}
	a.setMeta(meta)
	return nil
}


func (a *threadActor) loadLatestAssistantText(threadID int64) (string, error) {
	items, err := a.store.ListItems(a.ctx, threadID, threadstore.ListOptions{Limit: 100})
	if err != nil {
		return "", err
	}

	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Direction != "output" || item.ItemType != "message" {
			continue
		}

		text, err := extractAssistantTextFromItemPayload(item.Payload)
		if err != nil {
			return "", err
		}
		if text != "" {
			return text, nil
		}
	}

	return "", nil
}

// loadLatestToolCallArgs walks the child's output items in reverse and
// returns the arguments string of the most recent function_call. Used by
// publishChildInvocationResult to surface forced-tool-call output (e.g.
// the evidence locator's emit_bboxes call) up to the parent barrier.
// Returns "" without error when the child never emitted a function_call.
func (a *threadActor) loadLatestToolCallArgs(threadID int64) (string, error) {
	items, err := a.store.ListItems(a.ctx, threadID, threadstore.ListOptions{Limit: 100})
	if err != nil {
		return "", err
	}

	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Direction != "output" || item.ItemType != "function_call" {
			continue
		}

		var payload struct {
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return "", fmt.Errorf("decode function_call item: %w", err)
		}
		if strings.TrimSpace(payload.Arguments) != "" {
			return payload.Arguments, nil
		}
	}

	return "", nil
}

func extractAssistantTextFromItemPayload(raw json.RawMessage) (string, error) {
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode assistant message item: %w", err)
	}

	parts := make([]string, 0, len(payload.Content))
	for _, content := range payload.Content {
		if content.Type == "output_text" && content.Text != "" {
			parts = append(parts, content.Text)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

func (a *threadActor) setMeta(meta threadstore.ThreadMeta) {
	a.mu.Lock()
	a.meta = meta
	a.mu.Unlock()
}

func (a *threadActor) currentMeta() threadstore.ThreadMeta {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.meta
}

func (a *threadActor) releaseTerminalChildResources(meta *threadstore.ThreadMeta) error {
	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession("terminal_release")

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
	}

	meta.OwnerWorkerID = 0
	meta.SocketExpiresAt = time.Time{}
	a.setMeta(*meta)

	a.logger.Info("released terminal thread socket",
		appendThreadGraphAttrs([]any{
			"socket_generation", meta.SocketGeneration,
		}, *meta)...,
	)

	return nil
}

func (a *threadActor) clearCompletedInvocationState(meta *threadstore.ThreadMeta) error {
	if !shouldClearCompletedInvocationState(*meta) {
		return nil
	}

	meta.ParentCallID = ""
	meta.ActiveSpawnGroupID = 0
	meta.UpdatedAt = time.Now().UTC()
	return a.saveThreadMeta(*meta)
}

func (a *threadActor) currentSocketGeneration() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.meta.SocketGeneration
}

func (a *threadActor) currentOpenAISocketID() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.openAISocketID
}

func socketObservationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (a *threadActor) registerOpenAISocket(meta threadstore.ThreadMeta, session *openaiws.Session, heartbeatExpiresAt time.Time) string {
	if a.store == nil || session == nil {
		return ""
	}

	socketID, err := idgen.New("socket")
	if err != nil {
		a.logger.Warn("failed to generate openai socket session id", "error", err)
		return ""
	}

	snapshot := session.Snapshot()
	connectedAt := snapshot.ConnectedAt.UTC()
	if connectedAt.IsZero() {
		connectedAt = time.Now().UTC()
	}

	record := threadstore.OpenAISocketSession{
		ID:                     socketID,
		ThreadID:               meta.ID,
		RootThreadID:           defaultRootThreadID(meta),
		ParentThreadID:         meta.ParentThreadID,
		WorkerID:               a.workerID,
		ThreadSocketGeneration: meta.SocketGeneration,
		State:                  threadstore.OpenAISocketStateConnected,
		ConnectedAt:            connectedAt,
		LastReadAt:             snapshot.LastReadAt.UTC(),
		LastWriteAt:            snapshot.LastWriteAt.UTC(),
		LastHeartbeatAt:        connectedAt,
		HeartbeatExpiresAt:     heartbeatExpiresAt.UTC(),
		CreatedAt:              connectedAt,
		UpdatedAt:              connectedAt,
	}

	ctx, cancel := socketObservationContext()
	defer cancel()
	if err := a.store.CreateOpenAISocketSession(ctx, record); err != nil {
		a.logger.Warn("failed to persist openai socket session",
			appendThreadGraphAttrs([]any{
				"socket_id", socketID,
				"socket_generation", meta.SocketGeneration,
				"error", err,
			}, meta)...,
		)
		return ""
	}

	return socketID
}

func (a *threadActor) touchOpenAISocket(session *openaiws.Session, touch threadstore.OpenAISocketTouch) {
	if a.store == nil || session == nil {
		return
	}

	socketID := strings.TrimSpace(a.currentOpenAISocketID())
	if socketID == "" {
		return
	}

	snapshot := session.Snapshot()
	touch.ID = socketID
	if touch.LastReadAt.IsZero() {
		touch.LastReadAt = snapshot.LastReadAt.UTC()
	}
	if touch.LastWriteAt.IsZero() {
		touch.LastWriteAt = snapshot.LastWriteAt.UTC()
	}
	if touch.LastReadAt.IsZero() && touch.LastWriteAt.IsZero() && touch.LastHeartbeatAt.IsZero() && touch.HeartbeatExpiresAt.IsZero() {
		return
	}

	ctx, cancel := socketObservationContext()
	defer cancel()
	if err := a.store.TouchOpenAISocketSession(ctx, touch); err != nil {
		a.logger.Warn("failed to touch openai socket session",
			"socket_id", socketID,
			"error", err,
		)
	}
}

func (a *threadActor) disconnectOpenAISocket(socketID, reason string) {
	if a.store == nil || strings.TrimSpace(socketID) == "" {
		return
	}

	now := time.Now().UTC()
	ctx, cancel := socketObservationContext()
	defer cancel()
	if err := a.store.DisconnectOpenAISocketSession(ctx, socketID, reason, now, now.Add(socketPruneTTL)); err != nil {
		a.logger.Warn("failed to disconnect openai socket session",
			"socket_id", socketID,
			"reason", reason,
			"error", err,
		)
	}
}

func (a *threadActor) resetSession(reason string) {
	a.mu.Lock()
	session := a.session
	socketID := a.openAISocketID
	a.session = nil
	a.openAISocketID = ""
	a.mu.Unlock()

	if session != nil {
		_ = a.closeAndRelease(session)
	}
	a.disconnectOpenAISocket(socketID, reason)
}

func decodeJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode json array: %w", err)
	}

	return items, nil
}

func wrapRawItemAsArray(raw json.RawMessage) (json.RawMessage, error) {
	items := []json.RawMessage{raw}
	payload, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("marshal single-item input array: %w", err)
	}

	return payload, nil
}

func normalizeRawItemsAsArray(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return wrapRawItemAsArray(trimmed)
	}
	if trimmed[0] == '[' {
		if _, err := decodeJSONArray(trimmed); err != nil {
			return nil, err
		}
		return trimmed, nil
	}
	return wrapRawItemAsArray(trimmed)
}

func itemTypeFromRaw(raw json.RawMessage) string {
	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return "unknown"
	}

	if item.Type == "" {
		return "unknown"
	}

	return item.Type
}

func (a *threadActor) logOpenAIEvent(meta threadstore.ThreadMeta, responseID string, attrs ...any) {
	a.logger.Info("received openai event", appendOpenAIEventLogAttrs(attrs, meta, responseID)...)
}

func appendOpenAIEventLogAttrs(attrs []any, meta threadstore.ThreadMeta, responseID string) []any {
	if strings.TrimSpace(responseID) != "" && !hasLogAttrKey(attrs, "response_id") {
		attrs = append(attrs, "response_id", responseID)
	}
	return appendThreadGraphAttrs(attrs, meta)
}

func (a *threadActor) logOutputItemEvent(meta threadstore.ThreadMeta, responseID string, event openaiws.ServerEvent) {
	itemRaw := outputItemEventItemRaw(event)
	attrs := []any{"event_type", outputItemLogEventType(event)}
	if sequenceNumber, ok := outputItemEventSequenceNumber(event); ok {
		attrs = append(attrs, "event_sequence_number", sequenceNumber)
	}
	attrs = append(attrs, outputItemSemanticAttrs(itemRaw)...)
	a.logOpenAIEvent(meta, responseID, attrs...)
}

func outputItemLogEventType(event openaiws.ServerEvent) string {
	if event.Type != openaiws.EventTypeResponseOutputItemAdded && event.Type != openaiws.EventTypeResponseOutputItemDone {
		return string(event.Type)
	}

	itemRaw := outputItemEventItemRaw(event)
	itemType := itemTypeFromRaw(itemRaw)
	if itemType == "" || itemType == "unknown" {
		return string(event.Type)
	}

	return string(event.Type) + "." + itemType
}

func outputItemEventSequenceNumber(event openaiws.ServerEvent) (int64, bool) {
	var payload struct {
		SequenceNumber int64 `json:"sequence_number"`
	}
	if len(event.Raw) > 0 {
		if err := json.Unmarshal(event.Raw, &payload); err == nil && payload.SequenceNumber > 0 {
			return payload.SequenceNumber, true
		}
	}

	raw := event.Field("sequence_number")
	if len(raw) == 0 {
		return 0, false
	}

	var sequenceNumber int64
	if err := json.Unmarshal(raw, &sequenceNumber); err != nil {
		return 0, false
	}

	return sequenceNumber, true
}

func outputItemEventItemRaw(event openaiws.ServerEvent) json.RawMessage {
	itemRaw := event.Field("item")
	if len(itemRaw) == 0 && len(event.Raw) > 0 {
		var payload struct {
			Item json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(event.Raw, &payload); err == nil {
			itemRaw = payload.Item
		}
	}
	return itemRaw
}

func outputItemSemanticAttrs(itemRaw json.RawMessage) []any {
	if len(itemRaw) == 0 {
		return nil
	}

	itemType := itemTypeFromRaw(itemRaw)
	if itemType != "function_call" {
		return nil
	}

	call, err := parseFunctionCallItem(itemRaw)
	if err != nil {
		return nil
	}

	attrs := make([]any, 0, 8)
	if strings.TrimSpace(call.CallID) != "" {
		attrs = append(attrs, "call_id", call.CallID)
	}
	if strings.TrimSpace(call.Name) != "" {
		attrs = append(attrs,
			"call_name", call.Name,
			"call_kind", classifyFunctionCallName(call.Name),
		)
	}

	switch call.Name {
	case doccmd.ToolNameQueryDocument:
		if req, ok := decodeDocQueryRequestForLog(call.Arguments); ok {
			attrs = append(attrs, "document_id", req.DocumentID)
		}
	case "spawn_threads":
		if req, ok := decodeSpawnRequestForLog(call.Arguments); ok {
			attrs = append(attrs, "child_count", len(req.Children))
			if spawnMode := normalizeSpawnMode(req.SpawnMode); spawnMode != "" {
				attrs = append(attrs, "spawn_mode", spawnMode)
			}
		}
	}

	return attrs
}

func classifyFunctionCallName(name string) string {
	switch strings.TrimSpace(name) {
	case doccmd.ToolNameQueryDocument:
		return "document_query"
	case "spawn_threads":
		return "spawn_threads"
	case "":
		return "tool"
	default:
		return "tool"
	}
}

func decodeDocQueryRequestForLog(arguments string) (docQueryRequest, bool) {
	var req docQueryRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return docQueryRequest{}, false
	}
	return req, req.DocumentID > 0
}

func decodeSpawnRequestForLog(arguments string) (spawnRequest, bool) {
	var req spawnRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return spawnRequest{}, false
	}
	return req, len(req.Children) > 0
}

func extractReasoningEffort(reasoningJSON string) string {
	trimmed := strings.TrimSpace(reasoningJSON)
	if trimmed == "" {
		return ""
	}
	var param shared.ReasoningParam
	if err := json.Unmarshal([]byte(trimmed), &param); err != nil {
		return ""
	}
	return string(param.Effort)
}

func responseCreateReasoningEffort(payload map[string]any) string {
	switch reasoning := payload["reasoning"].(type) {
	case map[string]any:
		effort, _ := reasoning["effort"].(string)
		return effort
	case shared.ReasoningParam:
		return string(reasoning.Effort)
	case *shared.ReasoningParam:
		if reasoning == nil {
			return ""
		}
		return string(reasoning.Effort)
	default:
		return ""
	}
}

func statusSupportsIdleSocket(status threadstore.ThreadStatus) bool {
	switch status {
	case threadstore.ThreadStatusReady, threadstore.ThreadStatusWaitingTool, threadstore.ThreadStatusWaitingChildren:
		return true
	default:
		return false
	}
}

func summarizeSpawnResults(results []threadstore.SpawnChildResult) (completed, failed, cancelled int) {
	for _, result := range results {
		switch result.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		}
	}

	return completed, failed, cancelled
}

// aggregateSpawnOutputItemForGroup dispatches aggregation on the spawn
// group's kind. citation_locator groups need a finalize RPC round-trip
// per child (to persist rows + build per-call function_call_output);
// everything else falls through to the existing assistant-text aggregation.
func (a *threadActor) aggregateSpawnOutputItemForGroup(spawn threadstore.SpawnGroupMeta, results []threadstore.SpawnChildResult) (json.RawMessage, error) {
	if spawn.GroupKind == "citation_locator" {
		return a.aggregateCitationLocatorOutputs(spawn, results)
	}
	return aggregateSpawnOutputItem(spawn, results)
}

func aggregateSpawnOutputItem(spawn threadstore.SpawnGroupMeta, results []threadstore.SpawnChildResult) (json.RawMessage, error) {
	children := make([]map[string]any, 0, len(results))
	for _, result := range results {
		switch result.Status {
		case "completed", "failed", "cancelled":
		default:
			continue
		}

		child := map[string]any{
			"thread_id":   result.ChildThreadID,
			"response_id": result.ChildResponseID,
			"status":      result.Status,
		}
		if result.AssistantText != "" {
			child["assistant_text"] = result.AssistantText
		}
		if result.ResultRef != "" {
			child["result_ref"] = result.ResultRef
		}
		if result.SummaryRef != "" {
			child["summary_ref"] = result.SummaryRef
		}
		if result.ErrorRef != "" {
			child["error_ref"] = result.ErrorRef
		}
		if result.DocumentID > 0 {
			child["document_id"] = result.DocumentID
		}
		children = append(children, child)
	}

	callBindings, err := decodeDocQueryRoundCalls(spawn.ParentCallID)
	if err != nil {
		return nil, err
	}
	if len(callBindings) == 0 {
		return nil, fmt.Errorf("spawn group %d has no parent call bindings", spawn.ID)
	}

	outputItems := make([]map[string]any, 0, len(callBindings))
	for _, binding := range callBindings {
		filteredChildren := children
		if binding.DocumentID > 0 {
			filteredChildren = make([]map[string]any, 0, len(children))
			for _, child := range children {
				var documentID int64
				switch v := child["document_id"].(type) {
				case int64:
					documentID = v
				case float64:
					documentID = int64(v)
				}
				if documentID == binding.DocumentID {
					filteredChildren = append(filteredChildren, child)
				}
			}
		}

		outputPayload := map[string]any{
			"spawn_group_id": spawn.ID,
			"children":       filteredChildren,
		}
		if binding.DocumentID > 0 {
			outputPayload["document_id"] = binding.DocumentID
		}
		if strings.TrimSpace(binding.Task) != "" {
			outputPayload["task"] = binding.Task
		}

		outputJSON, err := json.Marshal(outputPayload)
		if err != nil {
			return nil, fmt.Errorf("marshal aggregate spawn output payload: %w", err)
		}

		outputItems = append(outputItems, map[string]any{
			"type":    "function_call_output",
			"call_id": binding.CallID,
			"output":  string(outputJSON),
		})
	}

	if len(outputItems) == 1 {
		payload, err := json.Marshal(outputItems[0])
		if err != nil {
			return nil, fmt.Errorf("marshal aggregate spawn output item: %w", err)
		}
		return payload, nil
	}

	payload, err := json.Marshal(outputItems)
	if err != nil {
		return nil, fmt.Errorf("marshal aggregate spawn output items: %w", err)
	}
	return payload, nil
}

type spawnRequest struct {
	SpawnMode              string           `json:"spawn_mode,omitempty"`
	BranchPreviousResponse string           `json:"branch_previous_response_id,omitempty"`
	InheritInstructions    *bool            `json:"inherit_instructions,omitempty"`
	InheritTools           *bool            `json:"inherit_tools,omitempty"`
	Children               []spawnChildSpec `json:"children"`
}

type spawnChildSpec struct {
	ThreadID     int64           `json:"thread_id,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Prompt       string          `json:"prompt,omitempty"`
	Model        string          `json:"model,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func parseFunctionCallItem(raw json.RawMessage) (functionCallItem, error) {
	var item functionCallItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return functionCallItem{}, fmt.Errorf("decode function_call item: %w", err)
	}
	if item.Type != "function_call" {
		return functionCallItem{}, fmt.Errorf("item type is %q, want function_call", item.Type)
	}
	return item, nil
}

func decodeSpawnRequest(arguments string, parentMeta threadstore.ThreadMeta) (spawnRequest, error) {
	var request spawnRequest
	if err := json.Unmarshal([]byte(arguments), &request); err != nil {
		return spawnRequest{}, fmt.Errorf("decode spawn_threads arguments: %w", err)
	}

	if len(request.Children) == 0 {
		return spawnRequest{}, fmt.Errorf("spawn_threads requires at least one child")
	}

	if len(request.Children) > maxChildrenPerSpawn {
		return spawnRequest{}, fmt.Errorf("spawn_threads requested %d children but the per-turn cap is %d", len(request.Children), maxChildrenPerSpawn)
	}

	if parentMeta.Depth >= 1 {
		return spawnRequest{}, fmt.Errorf("spawn_threads depth limit reached for thread %d", parentMeta.ID)
	}
	switch normalizeSpawnMode(request.SpawnMode) {
	case spawnModeCold, spawnModeWarmBranch:
	default:
		return spawnRequest{}, fmt.Errorf("unsupported spawn_mode %q", request.SpawnMode)
	}

	for index, child := range request.Children {
		if len(child.Input) == 0 && child.Prompt == "" {
			return spawnRequest{}, fmt.Errorf("child %d is missing input or prompt", index)
		}
	}

	return request, nil
}

// executePendingPageReads fires a PrepareInput RPC for each page-read call,
// loads the resulting function_call_output artifacts, and returns the
// concatenated item array as the input for the next response.create. Calls are
// processed sequentially for simplicity; parallelization can come later if the
// RPC becomes a latency bottleneck. Per-call errors (unattached doc, unknown
// page, RPC failure) surface as error-shaped function_call_output items so the
// model can reason about them instead of the whole thread dying.
func (a *threadActor) executePendingPageReads(meta threadstore.ThreadMeta, calls []pageReadCall) (json.RawMessage, error) {
	if len(calls) == 0 {
		return nil, fmt.Errorf("executePendingPageReads called with no pending calls")
	}
	if a.preparedInputs == nil {
		return nil, fmt.Errorf("prepared input client not configured")
	}
	if a.blob == nil {
		return nil, fmt.Errorf("blob store not configured")
	}

	store, err := preparedinput.NewStore(a.blob)
	if err != nil {
		return nil, fmt.Errorf("open prepared input store: %w", err)
	}

	// Validate attachment once for all requested docs.
	attachedSet := map[int64]bool{}
	if a.threadDocs != nil {
		docIDs := make([]int64, 0, len(calls))
		for _, c := range calls {
			docIDs = append(docIDs, c.Request.DocumentID)
		}
		attached, err := a.threadDocs.FilterAttached(a.ctx, meta.ID, docIDs)
		if err != nil {
			return nil, fmt.Errorf("validate attached documents: %w", err)
		}
		for _, id := range attached {
			attachedSet[id] = true
		}
	}

	items := make([]any, 0, len(calls))
	for _, call := range calls {
		if !attachedSet[call.Request.DocumentID] {
			items = append(items, errorFunctionCallOutput(call.CallID, fmt.Sprintf("document %d is not attached to this thread", call.Request.DocumentID)))
			continue
		}

		requestID := fmt.Sprintf("pr_%d_%d_%d_%d", meta.ID, call.Request.DocumentID, call.Request.PageNumber, time.Now().UTC().UnixNano())
		resp, err := a.preparedInputs.PrepareInput(a.ctx, doccmd.PrepareInputRequest{
			RequestID:  requestID,
			Kind:       doccmd.PrepareKindPageRead,
			ThreadID:   meta.ID,
			DocumentID: call.Request.DocumentID,
			PageNumber: call.Request.PageNumber,
			CallID:     call.CallID,
		})
		if err != nil {
			a.logger.Warn("page read prepare input failed",
				"call_id", call.CallID,
				"document_id", call.Request.DocumentID,
				"page_number", call.Request.PageNumber,
				"error", err,
			)
			items = append(items, errorFunctionCallOutput(call.CallID, fmt.Sprintf("failed to prepare page: %v", err)))
			continue
		}
		if strings.TrimSpace(resp.Status) != doccmd.PrepareStatusOK {
			msg := strings.TrimSpace(resp.Error)
			if msg == "" {
				msg = fmt.Sprintf("prepare input returned status %q", resp.Status)
			}
			items = append(items, errorFunctionCallOutput(call.CallID, msg))
			continue
		}

		artifact, err := store.Read(a.ctx, resp.PreparedInputRef)
		if err != nil {
			items = append(items, errorFunctionCallOutput(call.CallID, fmt.Sprintf("read prepared input: %v", err)))
			continue
		}

		var artifactItems []map[string]any
		if err := json.Unmarshal(artifact.Input, &artifactItems); err != nil || len(artifactItems) != 1 {
			items = append(items, errorFunctionCallOutput(call.CallID, "prepared page-read artifact was malformed"))
			continue
		}
		items = append(items, artifactItems[0])
	}

	raw, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("marshal page read input items: %w", err)
	}
	return raw, nil
}

func errorFunctionCallOutput(callID, message string) map[string]any {
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  "error: " + message,
	}
}

func (a *threadActor) startDocumentQueryGroup(parentMeta threadstore.ThreadMeta, calls []docQueryCall) (int64, error) {
	if a.threadDocs == nil {
		return 0, fmt.Errorf("document store not available")
	}
	roundCalls := normalizeDocQueryRoundCalls(calls)
	if len(roundCalls) == 0 {
		return 0, fmt.Errorf("document query round requires at least one tool call")
	}
	roundCallID := roundCalls[0].CallID
	docWork := buildDocQueryDocWork(roundCalls)

	allDocumentIDs := make([]int64, 0, len(docWork))
	for _, work := range docWork {
		allDocumentIDs = append(allDocumentIDs, work.DocumentID)
	}

	attached, err := a.threadDocs.FilterAttached(a.ctx, parentMeta.ID, allDocumentIDs)
	if err != nil {
		return 0, fmt.Errorf("validate attached documents: %w", err)
	}

	attachedSet := make(map[int64]bool, len(attached))
	for _, id := range attached {
		attachedSet[id] = true
	}
	var missing []int64
	for _, id := range allDocumentIDs {
		if !attachedSet[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: documents not attached to thread: %v", errCommandPrecond, missing)
	}

	childThreadIDs := make([]int64, 0, len(docWork))
	childCommands := make([]threadcmd.Command, 0, len(docWork))
	encodedParentCallID, err := encodeDocQueryRoundCalls(roundCalls)
	if err != nil {
		return 0, err
	}

	spawnMeta, err := a.store.LoadOrCreateSpawnGroup(a.ctx, threadstore.SpawnGroupMeta{
		ParentThreadID: parentMeta.ID,
		ParentCallID:   encodedParentCallID,
		GroupKind:      "document_query",
		StableKey:      roundCallID,
		Expected:       len(docWork),
		Status:         threadstore.SpawnGroupStatusWaiting,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
	if err != nil {
		return 0, err
	}
	spawnGroupID := spawnMeta.ID

	for _, work := range docWork {
		doc, err := a.docStore.Get(a.ctx, work.DocumentID)
		if err != nil {
			return 0, fmt.Errorf("load document %d: %w", work.DocumentID, err)
		}

		model := doc.QueryModel
		if model == "" {
			model = parentMeta.Model
		}

		reasoning := doc.QueryReasoning
		if reasoning == "" {
			reasoning = extractReasoningEffort(parentMeta.ReasoningJSON)
		}

		previousResponseID := ""
		lineageSource := "warmup"
		lineage, err := a.store.LoadLatestCompletedDocumentQueryLineage(a.ctx, parentMeta.ID, work.DocumentID)
		if err != nil && !errors.Is(err, threadstore.ErrThreadNotFound) {
			return 0, fmt.Errorf("get latest completed document child lineage for %d: %w", work.DocumentID, err)
		}
		if err == nil {
			previousResponseID = lineage.ResponseID
			lineageSource = "thread_local"
			if strings.TrimSpace(lineage.Model) != "" {
				model = lineage.Model
			}
		}
		if previousResponseID == "" && doc.BaseResponseID != "" && doc.BaseModel == model && doc.BaseReasoning == reasoning {
			previousResponseID = doc.BaseResponseID
			lineageSource = "document_base"
		}

		task := buildDocQueryDocTask(work.Calls)
		phase := "query"
		var preparedInputRef string
		if previousResponseID == "" {
			phase = "warmup"
			resp, err := a.preparedInputs.PrepareInput(a.ctx, doccmd.PrepareInputRequest{
				RequestID:  stableDocumentPreparedInputID(parentMeta.ID, roundCallID, work.DocumentID, phase),
				Kind:       doccmd.PrepareKindWarmup,
				ThreadID:   parentMeta.ID,
				DocumentID: work.DocumentID,
			})
			if err != nil {
				return 0, fmt.Errorf("prepare warmup input for document %d: %w", work.DocumentID, err)
			}
			if resp.Status != doccmd.PrepareStatusOK {
				return 0, fmt.Errorf("prepare warmup input for document %d failed: %s", work.DocumentID, resp.Error)
			}
			preparedInputRef = resp.PreparedInputRef
		}

		threadID, startCmd, reusedThread, err := a.buildDocumentChildStartCommand(parentMeta, spawnGroupID, roundCallID, work.DocumentID, doc.Filename, model, reasoning, phase, previousResponseID, preparedInputRef, task, 0)
		if err != nil {
			return 0, err
		}
		childCommands = append(childCommands, startCmd)
		childThreadIDs = append(childThreadIDs, threadID)

		childKind := "document_query"
		if phase == "warmup" {
			childKind = "document_warmup"
		}
		a.logger.Info("spawning child thread",
			appendThreadGraphAttrs([]any{
				"spawn_group_id", spawnGroupID,
				"child_thread_id", threadID,
				"child_kind", childKind,
				"document_id", work.DocumentID,
				"document_name", doc.Filename,
				"phase", phase,
				"model", model,
				"lineage_source", lineageSource,
				"has_previous_response_id", previousResponseID != "",
				"reuse_existing_thread", reusedThread,
			}, parentMeta)...,
		)
	}

	spawnMeta.ParentCallID = encodedParentCallID
	spawnMeta.Expected = len(childCommands)
	spawnMeta.GroupKind = "document_query"
	spawnMeta.StableKey = roundCallID
	if err := a.store.CreateSpawnGroup(a.ctx, spawnMeta, childThreadIDs); err != nil {
		return 0, err
	}

	a.logger.Info("opened child barrier",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawnGroupID,
			"child_source", "document_query",
			"expected_children", len(childCommands),
			"function_call_count", len(roundCalls),
		}, parentMeta)...,
	)

	for _, cmd := range childCommands {
		if err := a.publish(a.ctx, threadcmd.DispatchStartSubject, cmd); err != nil {
			return 0, err
		}
	}

	return spawnGroupID, nil
}

func normalizeDocQueryRoundCalls(calls []docQueryCall) []docQueryRoundCall {
	normalized := make([]docQueryRoundCall, 0, len(calls))
	for _, call := range calls {
		callID := strings.TrimSpace(call.CallID)
		if callID == "" {
			continue
		}
		if call.Request.DocumentID <= 0 {
			continue
		}
		normalized = append(normalized, docQueryRoundCall{
			CallID:     callID,
			DocumentID: call.Request.DocumentID,
			Task:       strings.TrimSpace(call.Request.Task),
		})
	}
	return normalized
}

func buildDocQueryDocWork(calls []docQueryRoundCall) []docQueryDocWork {
	byDocumentID := make(map[int64]*docQueryDocWork, len(calls))
	ordered := make([]docQueryDocWork, 0, len(calls))

	for _, call := range calls {
		if call.DocumentID <= 0 {
			continue
		}
		work, ok := byDocumentID[call.DocumentID]
		if !ok {
			ordered = append(ordered, docQueryDocWork{DocumentID: call.DocumentID})
			work = &ordered[len(ordered)-1]
			byDocumentID[call.DocumentID] = work
		}
		work.Calls = append(work.Calls, call)
	}

	return ordered
}

func buildDocQueryDocTask(calls []docQueryRoundCall) string {
	if len(calls) == 1 {
		return calls[0].Task
	}

	var builder strings.Builder
	builder.WriteString("Multiple query_document requests from the same parent response target this document. Respond with clearly labeled sections for each parent call.\n\n")
	builder.WriteString("<parent_calls>\n")
	for _, call := range calls {
		builder.WriteString(`<call id="`)
		builder.WriteString(docprompt.EscapeAttribute(call.CallID))
		builder.WriteString(`">`)
		builder.WriteByte('\n')
		builder.WriteString(call.Task)
		builder.WriteByte('\n')
		builder.WriteString("</call>\n")
	}
	builder.WriteString("</parent_calls>")
	return builder.String()
}

func encodeDocQueryRoundCalls(calls []docQueryRoundCall) (string, error) {
	if len(calls) == 0 {
		return "", fmt.Errorf("document query round requires at least one tool call")
	}
	if len(calls) == 1 {
		return calls[0].CallID, nil
	}

	payload, err := json.Marshal(map[string]any{
		"kind":  "document_query_round",
		"calls": calls,
	})
	if err != nil {
		return "", fmt.Errorf("marshal document query round calls: %w", err)
	}
	return string(payload), nil
}

func decodeDocQueryRoundCalls(raw string) ([]docQueryRoundCall, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "{") {
		return []docQueryRoundCall{{CallID: raw}}, nil
	}

	var payload struct {
		Kind  string              `json:"kind"`
		Calls []docQueryRoundCall `json:"calls"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("decode document query round calls: %w", err)
	}
	if payload.Kind != "document_query_round" {
		return []docQueryRoundCall{{CallID: raw}}, nil
	}
	return normalizeDocQueryRoundCalls(roundCallsToPendingCalls(payload.Calls)), nil
}

func roundCallsToPendingCalls(calls []docQueryRoundCall) []docQueryCall {
	pending := make([]docQueryCall, 0, len(calls))
	for _, call := range calls {
		pending = append(pending, docQueryCall{
			CallID: call.CallID,
			Request: docQueryRequest{
				DocumentID: call.DocumentID,
				Task:       call.Task,
			},
		})
	}
	return pending
}

func (a *threadActor) buildDocumentChildStartCommand(parentMeta threadstore.ThreadMeta, spawnGroupID int64, parentCallID string, documentID int64, documentName, model, reasoning, phase, previousResponseID, preparedInputRef, task string, bootstrapChildThreadID int64) (int64, threadcmd.Command, bool, error) {
	cmdID, err := a.store.LoadOrCreateCommandID(
		a.ctx,
		"thread",
		string(threadcmd.KindThreadStart),
		stableDocumentChildCommandDedupeKey(parentMeta.ID, parentCallID, documentID, phase),
	)
	if err != nil {
		return 0, threadcmd.Command{}, false, err
	}

	startBody := map[string]any{
		"model": model,
		"store": true,
	}
	if reasoning != "" {
		startBody["reasoning"] = map[string]any{"effort": reasoning}
	}
	if strings.TrimSpace(preparedInputRef) != "" {
		startBody["prepared_input_ref"] = preparedInputRef
	} else {
		startBody["initial_input"] = []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": task,
					},
				},
			},
		}
		startBody["previous_response_id"] = previousResponseID
	}

	metadataMap := map[string]string{
		"document_id":   formatDocumentIDLocal(documentID),
		"document_name": documentName,
	}
	switch phase {
	case "warmup":
		metadataMap["spawn_mode"] = "document_warmup"
		metadataMap["document_task"] = task
	default:
		metadataMap["spawn_mode"] = "document_query"
		if bootstrapChildThreadID > 0 {
			metadataMap["bootstrap_child_thread_id"] = formatThreadIDLocal(bootstrapChildThreadID)
		}
	}

	childMetadataJSON, err := json.Marshal(metadataMap)
	if err != nil {
		return 0, threadcmd.Command{}, false, fmt.Errorf("marshal child metadata: %w", err)
	}

	metadata, err := rawJSONToAny(childMetadataJSON)
	if err != nil {
		return 0, threadcmd.Command{}, false, fmt.Errorf("decode child metadata: %w", err)
	}
	startBody["metadata"] = metadata

	body, err := json.Marshal(startBody)
	if err != nil {
		return 0, threadcmd.Command{}, false, fmt.Errorf("marshal child start body: %w", err)
	}

	threadID, reusedThread, err := a.prepareDocumentChildThreadMeta(parentMeta, spawnGroupID, parentCallID, documentID, phase, model, string(childMetadataJSON))
	if err != nil {
		return 0, threadcmd.Command{}, false, err
	}

	causationID := parentMeta.LastResponseID
	if strings.TrimSpace(previousResponseID) != "" {
		causationID = previousResponseID
	}

	return threadID, threadcmd.Command{
		CmdID:        cmdID,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     threadID,
		RootThreadID: parentMeta.RootThreadID,
		CausationID:  causationID,
		Body:         body,
	}, reusedThread, nil
}

func (a *threadActor) prepareDocumentChildThreadMeta(parentMeta threadstore.ThreadMeta, spawnGroupID int64, parentCallID string, documentID int64, phase, model, metadataJSON string) (int64, bool, error) {
	now := time.Now().UTC()

	existing, err := a.store.LoadReusableDocumentChildThread(a.ctx, parentMeta.ID, documentID)
	if err != nil {
		if !errors.Is(err, threadstore.ErrThreadNotFound) {
			return 0, false, err
		}

		threadID, reserveErr := a.store.ReserveThreadID(a.ctx)
		if reserveErr != nil {
			return 0, false, reserveErr
		}

		if err := a.store.CreateThreadIfAbsent(a.ctx, threadstore.ThreadMeta{
			ID:                 threadID,
			RootThreadID:       parentMeta.RootThreadID,
			ParentThreadID:     parentMeta.ID,
			ParentCallID:       parentCallID,
			Depth:              parentMeta.Depth + 1,
			Status:             threadstore.ThreadStatusNew,
			Model:              model,
			MetadataJSON:       metadataJSON,
			ActiveSpawnGroupID: spawnGroupID,
			ChildKind:          "document",
			DocumentID:         documentID,
			DocumentPhase:      phase,
			CreatedAt:          now,
			UpdatedAt:          now,
		}); err != nil {
			return 0, false, err
		}
		return threadID, false, nil
	}

	if !canReuseDocumentChildThread(existing.Status) {
		return 0, false, fmt.Errorf("document child thread %d is not reusable in status %s", existing.ID, existing.Status)
	}

	existing.RootThreadID = parentMeta.RootThreadID
	existing.ParentThreadID = parentMeta.ID
	existing.ParentCallID = parentCallID
	existing.Depth = parentMeta.Depth + 1
	existing.ActiveSpawnGroupID = spawnGroupID
	existing.Model = model
	existing.MetadataJSON = metadataJSON
	existing.ChildKind = "document"
	existing.DocumentID = documentID
	existing.DocumentPhase = phase
	existing.UpdatedAt = now
	if existing.CreatedAt.IsZero() {
		existing.CreatedAt = now
	}

	if err := a.store.SaveThread(a.ctx, existing); err != nil {
		return 0, false, err
	}

	return existing.ID, true, nil
}

func primaryDocQueryRoundCallID(raw string) string {
	calls, err := decodeDocQueryRoundCalls(raw)
	if err == nil && len(calls) > 0 {
		return calls[0].CallID
	}
	return strings.TrimSpace(raw)
}

func canReuseDocumentChildThread(status threadstore.ThreadStatus) bool {
	switch status {
	case threadstore.ThreadStatusNew,
		threadstore.ThreadStatusReady,
		threadstore.ThreadStatusCompleted,
		threadstore.ThreadStatusFailed,
		threadstore.ThreadStatusCancelled,
		threadstore.ThreadStatusIncomplete:
		return true
	default:
		return false
	}
}

func stableDocumentChildCommandDedupeKey(parentThreadID int64, parentCallID string, documentID int64, phase string) string {
	return stableDocumentDerivedID("thread_cmd_doc", formatThreadIDLocal(parentThreadID), parentCallID, formatDocumentIDLocal(documentID), phase, "start")
}

func stableDocumentPreparedInputID(parentThreadID int64, parentCallID string, documentID int64, phase string) string {
	return stableDocumentDerivedID("prep_doc", formatThreadIDLocal(parentThreadID), parentCallID, formatDocumentIDLocal(documentID), phase)
}

func stableDocumentDerivedID(prefix string, parts ...string) string {
	var builder strings.Builder
	builder.WriteString(prefix)
	for _, part := range parts {
		sanitized := sanitizeStableDocumentIDPart(part)
		if sanitized == "" {
			continue
		}
		builder.WriteByte('_')
		builder.WriteString(sanitized)
	}
	return builder.String()
}

func sanitizeStableDocumentIDPart(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var builder strings.Builder
	lastUnderscore := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastUnderscore = false
		case r == '_' || r == '-':
			builder.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}

func formatDocumentIDLocal(id int64) string {
	return strconv.FormatInt(id, 10)
}

func formatThreadIDLocal(id int64) string {
	return strconv.FormatInt(id, 10)
}

func formatCommandIDLocal(id int64) string {
	return strconv.FormatInt(id, 10)
}

func parseMetadataDocumentID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, fmt.Errorf("document id must be greater than zero")
	}
	return id, nil
}

func decodeThreadMetadataJSON(raw string) (shared.Metadata, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return shared.Metadata{}, nil
	}
	return decodeMetadataParam(json.RawMessage(raw))
}

func (a *threadActor) startSpawnGroup(parentMeta threadstore.ThreadMeta, parentCallID string, request spawnRequest) (int64, error) {
	spawnGroupID, err := a.store.ReserveSpawnGroupID(a.ctx)
	if err != nil {
		return 0, err
	}

	childThreadIDs := make([]int64, 0, len(request.Children))
	childCommands := make([]threadcmd.Command, 0, len(request.Children))
	childToolsJSON, err := filterChildThreadTools(parentMeta.ToolsJSON)
	if err != nil {
		return 0, err
	}
	childToolChoiceJSON, err := filterChildThreadToolChoice(parentMeta.ToolChoiceJSON)
	if err != nil {
		return 0, err
	}
	spawnMode := normalizeSpawnMode(request.SpawnMode)
	branchPreviousResponseID, err := resolveBranchPreviousResponseID(request, parentMeta)
	if err != nil {
		return 0, err
	}

	for index, child := range request.Children {
		threadID := child.ThreadID
		if threadID <= 0 {
			threadID, err = a.store.ReserveThreadID(a.ctx)
			if err != nil {
				return 0, err
			}
		}

		childInput, err := normalizeChildInput(child)
		if err != nil {
			return 0, err
		}

		cmdID, err := a.store.ReserveCommandID(a.ctx, "thread", string(threadcmd.KindThreadStart))
		if err != nil {
			return 0, err
		}

		startBody := map[string]any{
			"initial_input": childInput,
			"model":         defaultString(child.Model, parentMeta.Model),
			"instructions":  defaultString(child.Instructions, parentMeta.Instructions),
			"store":         true,
		}
		childMetadataJSON := parentMeta.MetadataJSON
		if len(child.Metadata) > 0 {
			childMetadataJSON = string(child.Metadata)
		}
		childIncludeJSON := parentMeta.IncludeJSON
		if childIncludeJSON == "" {
			normalizedInclude, err := threadcmd.NormalizeInclude(nil)
			if err != nil {
				return 0, fmt.Errorf("normalize child include: %w", err)
			}
			childIncludeJSON = string(normalizedInclude)
		}
		childMetadataJSON, err = mergeMetadataJSON(childMetadataJSON, map[string]string{
			"spawn_mode":                spawnMode,
			"branch_parent_thread_id":   formatThreadIDLocal(parentMeta.ID),
			"branch_parent_response_id": branchPreviousResponseID,
			"branch_index":              strconv.Itoa(index + 1),
		}, spawnMode == spawnModeWarmBranch)
		if err != nil {
			return 0, fmt.Errorf("merge child metadata: %w", err)
		}
		if strings.TrimSpace(childMetadataJSON) != "" {
			metadata, err := rawJSONToAny(json.RawMessage(childMetadataJSON))
			if err != nil {
				return 0, fmt.Errorf("decode child metadata: %w", err)
			}
			startBody["metadata"] = metadata
		}
		if strings.TrimSpace(childIncludeJSON) != "" {
			include, err := rawJSONToAny(json.RawMessage(childIncludeJSON))
			if err != nil {
				return 0, fmt.Errorf("decode child include: %w", err)
			}
			startBody["include"] = include
		}
		if spawnMode == spawnModeWarmBranch {
			startBody["previous_response_id"] = branchPreviousResponseID
		}
		if strings.TrimSpace(childToolsJSON) != "" {
			tools, err := rawJSONToAny(json.RawMessage(childToolsJSON))
			if err != nil {
				return 0, fmt.Errorf("decode child tools: %w", err)
			}
			startBody["tools"] = tools
		}
		if strings.TrimSpace(childToolChoiceJSON) != "" {
			toolChoice, err := rawJSONToAny(json.RawMessage(childToolChoiceJSON))
			if err != nil {
				return 0, fmt.Errorf("decode child tool_choice: %w", err)
			}
			startBody["tool_choice"] = toolChoice
		}
		if strings.TrimSpace(parentMeta.ReasoningJSON) != "" {
			reasoning, err := rawJSONToAny(json.RawMessage(parentMeta.ReasoningJSON))
			if err != nil {
				return 0, fmt.Errorf("decode child reasoning: %w", err)
			}
			startBody["reasoning"] = reasoning
		}

		body, err := json.Marshal(startBody)
		if err != nil {
			return 0, fmt.Errorf("marshal child start body: %w", err)
		}

		childCommands = append(childCommands, threadcmd.Command{
			CmdID:        cmdID,
			Kind:         threadcmd.KindThreadStart,
			ThreadID:     threadID,
			RootThreadID: parentMeta.RootThreadID,
			CausationID:  parentMeta.LastResponseID,
			Body:         body,
		})
		childThreadIDs = append(childThreadIDs, threadID)

		if err := a.store.CreateThreadIfAbsent(a.ctx, threadstore.ThreadMeta{
			ID:                 threadID,
			RootThreadID:       parentMeta.RootThreadID,
			ParentThreadID:     parentMeta.ID,
			ParentCallID:       parentCallID,
			Depth:              parentMeta.Depth + 1,
			Status:             threadstore.ThreadStatusNew,
			Model:              defaultString(child.Model, parentMeta.Model),
			Instructions:       defaultString(child.Instructions, parentMeta.Instructions),
			MetadataJSON:       childMetadataJSON,
			IncludeJSON:        childIncludeJSON,
			ToolsJSON:          childToolsJSON,
			ToolChoiceJSON:     childToolChoiceJSON,
			ReasoningJSON:      parentMeta.ReasoningJSON,
			ActiveSpawnGroupID: spawnGroupID,
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		}); err != nil {
			return 0, err
		}

		a.logger.Info("spawning child thread",
			appendThreadGraphAttrs([]any{
				"spawn_group_id", spawnGroupID,
				"child_thread_id", threadID,
				"child_kind", "thread",
				"spawn_mode", spawnMode,
				"child_model", defaultString(child.Model, parentMeta.Model),
				"branch_previous_response_id", strings.TrimSpace(branchPreviousResponseID) != "",
			}, parentMeta)...,
		)
	}

	if err := a.store.CreateSpawnGroup(a.ctx, threadstore.SpawnGroupMeta{
		ID:             spawnGroupID,
		ParentThreadID: parentMeta.ID,
		ParentCallID:   parentCallID,
		GroupKind:      "thread_spawn",
		Expected:       len(childCommands),
		Status:         threadstore.SpawnGroupStatusWaiting,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}, childThreadIDs); err != nil {
		return 0, err
	}

	a.logger.Info("opened child barrier",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawnGroupID,
			"child_source", "spawn_threads",
			"spawn_mode", spawnMode,
			"expected_children", len(childCommands),
		}, parentMeta)...,
	)

	for _, cmd := range childCommands {
		if err := a.publish(a.ctx, threadcmd.DispatchStartSubject, cmd); err != nil {
			return 0, err
		}
	}

	return spawnGroupID, nil
}

func (a *threadActor) publishChildInvocationResult(meta threadstore.ThreadMeta, resultStatus string) error {
	if a.publish == nil || meta.ParentThreadID <= 0 || meta.ActiveSpawnGroupID <= 0 {
		return nil
	}

	parentMeta, err := a.store.LoadThread(a.ctx, meta.ParentThreadID)
	if err != nil {
		return err
	}

	assistantText, err := a.loadLatestAssistantText(meta.ID)
	if err != nil {
		a.logger.Warn("failed to load child assistant summary",
			"spawn_group_id", meta.ActiveSpawnGroupID,
			"error", err,
		)
		assistantText = ""
	}

	toolCallArgs, err := a.loadLatestToolCallArgs(meta.ID)
	if err != nil {
		a.logger.Warn("failed to load child tool call arguments",
			"spawn_group_id", meta.ActiveSpawnGroupID,
			"error", err,
		)
		toolCallArgs = ""
	}

	body, err := json.Marshal(threadcmd.ChildResultBody{
		SpawnGroupID:     meta.ActiveSpawnGroupID,
		ChildThreadID:    meta.ID,
		ChildResponseID:  meta.LastResponseID,
		Status:           resultStatus,
		AssistantText:    assistantText,
		ToolCallArgsJSON: toolCallArgs,
	})
	if err != nil {
		return fmt.Errorf("marshal child terminal body: %w", err)
	}

	kind := threadcmd.KindThreadChildCompleted
	if resultStatus != "completed" {
		kind = threadcmd.KindThreadChildFailed
	}

	cmdID, err := a.store.ReserveCommandID(a.ctx, "thread", string(kind))
	if err != nil {
		return err
	}

	subject := threadcmd.DispatchAdoptSubject
	if parentMeta.OwnerWorkerID > 0 {
		subject = threadcmd.WorkerCommandSubject(parentMeta.OwnerWorkerID, kind)
	} else {
		switch kind {
		case threadcmd.KindThreadChildCompleted:
			subject = "thread.dispatch.child_completed"
		default:
			subject = "thread.dispatch.child_failed"
		}
	}

	return a.publish(a.ctx, subject, threadcmd.Command{
		CmdID:        cmdID,
		Kind:         kind,
		ThreadID:     meta.ParentThreadID,
		RootThreadID: parentMeta.RootThreadID,
		CausationID:  meta.LastResponseID,
		Body:         body,
	})
}

func normalizeChildInput(child spawnChildSpec) (json.RawMessage, error) {
	if len(child.Input) > 0 {
		return normalizeInputItems(child.Input)
	}

	payload, err := json.Marshal([]map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{{
			"type": "input_text",
			"text": child.Prompt,
		}},
	}})
	if err != nil {
		return nil, fmt.Errorf("marshal child prompt input: %w", err)
	}

	return payload, nil
}

func normalizeInputItems(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("input is required")
	}

	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("decode input array: %w", err)
		}
		return json.RawMessage(trimmed), nil
	case '{':
		return wrapRawItemAsArray(trimmed)
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("decode input text: %w", err)
		}
		payload, err := json.Marshal([]map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": text,
			}},
		}})
		if err != nil {
			return nil, fmt.Errorf("marshal input text: %w", err)
		}
		return payload, nil
	default:
		return nil, fmt.Errorf("input must be an array, an item object, or a JSON string")
	}
}

func isBlankInputJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

const (
	spawnModeCold       = "cold_spawn"
	spawnModeWarmBranch = "warm_branch"
)

func normalizeSpawnMode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return spawnModeCold
	}
	return raw
}

func resolveBranchPreviousResponseID(request spawnRequest, parentMeta threadstore.ThreadMeta) (string, error) {
	if normalizeSpawnMode(request.SpawnMode) != spawnModeWarmBranch {
		return "", nil
	}

	previousResponseID := strings.TrimSpace(request.BranchPreviousResponse)
	if previousResponseID == "" {
		previousResponseID = strings.TrimSpace(parentMeta.LastResponseID)
	}
	if previousResponseID == "" {
		return "", fmt.Errorf("warm_branch requires branch_previous_response_id or parent last_response_id")
	}

	return previousResponseID, nil
}

func mergeMetadataJSON(existing string, extra map[string]string, enabled bool) (string, error) {
	existing = strings.TrimSpace(existing)
	if !enabled && existing == "" {
		return "", nil
	}

	payload := shared.Metadata{}
	if existing != "" {
		decoded, err := decodeMetadataParam(json.RawMessage(existing))
		if err != nil {
			return "", fmt.Errorf("decode metadata json: %w", err)
		}
		payload = decoded
	}
	if enabled {
		for key, value := range extra {
			payload[key] = value
		}
	}
	if len(payload) == 0 {
		return "", nil
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal merged metadata json: %w", err)
	}

	return string(raw), nil
}

func filterChildThreadTools(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	tools, err := threadcmd.DecodeTools(json.RawMessage(raw))
	if err != nil {
		return "", fmt.Errorf("decode tools for child filtering: %w", err)
	}

	filtered := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		if isInternalRuntimeToolName(toolParamName(tool)) {
			continue
		}
		filtered = append(filtered, tool)
	}

	if len(filtered) == 0 {
		return "", nil
	}

	payload, err := json.Marshal(filtered)
	if err != nil {
		return "", fmt.Errorf("marshal filtered child tools: %w", err)
	}

	return string(payload), nil
}

func filterChildThreadToolChoice(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	choice, err := decodeToolChoiceParam(json.RawMessage(raw))
	if err != nil {
		return "", fmt.Errorf("decode tool_choice for child filtering: %w", err)
	}

	filtered, ok := filterChildThreadToolChoiceParam(choice)
	if !ok {
		return "", nil
	}

	payload, err := json.Marshal(filtered)
	if err != nil {
		return "", fmt.Errorf("marshal filtered child tool_choice: %w", err)
	}

	return string(payload), nil
}

func shouldReleaseTerminalChildResources(meta threadstore.ThreadMeta) bool {
	return meta.ParentThreadID > 0 && isTerminalThreadStatus(meta.Status)
}

func shouldPublishChildInvocationResult(meta threadstore.ThreadMeta, resultStatus string, ok bool) bool {
	return ok && meta.ParentThreadID > 0 && meta.ActiveSpawnGroupID > 0 && strings.TrimSpace(resultStatus) != ""
}

func successfulResponseThreadStatus(meta threadstore.ThreadMeta) threadstore.ThreadStatus {
	if meta.ParentThreadID <= 0 {
		return threadstore.ThreadStatusReady
	}
	if isReusableDocumentQueryChild(meta) {
		return threadstore.ThreadStatusReady
	}
	return threadstore.ThreadStatusCompleted
}

func childInvocationResultStatus(status threadstore.ThreadStatus, explicit string) (string, bool) {
	if normalized := strings.TrimSpace(explicit); normalized != "" {
		return normalized, true
	}

	switch status {
	case threadstore.ThreadStatusCompleted:
		return "completed", true
	case threadstore.ThreadStatusCancelled:
		return "cancelled", true
	case threadstore.ThreadStatusFailed, threadstore.ThreadStatusIncomplete:
		return "failed", true
	default:
		return "", false
	}
}

func childInvocationResultStatusForTerminalEvent(eventType openaiws.EventType) string {
	switch eventType {
	case openaiws.EventTypeResponseCompleted:
		return "completed"
	case openaiws.EventTypeResponseFailed, openaiws.EventTypeResponseIncomplete:
		return "failed"
	default:
		return ""
	}
}

func isReusableDocumentQueryChild(meta threadstore.ThreadMeta) bool {
	if meta.ParentThreadID <= 0 {
		return false
	}

	metadata, err := decodeThreadMetadataJSON(meta.MetadataJSON)
	if err != nil {
		return false
	}

	return strings.TrimSpace(metadata["spawn_mode"]) == "document_query" &&
		strings.TrimSpace(metadata["document_id"]) != ""
}

func shouldClearCompletedInvocationState(meta threadstore.ThreadMeta) bool {
	if meta.ActiveSpawnGroupID <= 0 {
		return false
	}

	return meta.Status == threadstore.ThreadStatusReady || isTerminalThreadStatus(meta.Status)
}

func isTerminalThreadStatus(status threadstore.ThreadStatus) bool {
	switch status {
	case threadstore.ThreadStatusCompleted, threadstore.ThreadStatusFailed, threadstore.ThreadStatusCancelled, threadstore.ThreadStatusIncomplete:
		return true
	default:
		return false
	}
}

func validateCommandPreconditions(cmd threadcmd.Command, meta threadstore.ThreadMeta) error {
	if cmd.ExpectedStatus != "" && string(meta.Status) != cmd.ExpectedStatus {
		return fmt.Errorf("%w: expected status %s, got %s", errCommandPrecond, cmd.ExpectedStatus, meta.Status)
	}

	if cmd.ExpectedLastResponseID != "" && meta.LastResponseID != cmd.ExpectedLastResponseID {
		return fmt.Errorf("%w: expected last_response_id %s, got %s", errCommandPrecond, cmd.ExpectedLastResponseID, meta.LastResponseID)
	}

	if cmd.ExpectedSocketGeneration > 0 && meta.SocketGeneration != cmd.ExpectedSocketGeneration {
		return fmt.Errorf("%w: expected socket_generation %d, got %d", errCommandPrecond, cmd.ExpectedSocketGeneration, meta.SocketGeneration)
	}

	return nil
}

func extractResponseCreatePayload(raw json.RawMessage) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode client response.create event: %w", err)
	}

	rawType, ok := payload["type"].(string)
	if !ok {
		return nil, fmt.Errorf("response.create event is missing type")
	}
	if openaiws.EventType(rawType) != openaiws.EventTypeResponseCreate {
		return nil, fmt.Errorf("event type is %q, want %q", rawType, openaiws.EventTypeResponseCreate)
	}

	delete(payload, "type")
	delete(payload, "event_id")

	if nested, ok := payload["response"].(map[string]any); ok {
		return nested, nil
	}

	return payload, nil
}
