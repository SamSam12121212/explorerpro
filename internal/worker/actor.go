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

	"explorer/internal/agentcmd"
	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/idgen"
	"explorer/internal/openaiws"
	"explorer/internal/threadevents"
	"explorer/internal/threadhistory"
	"explorer/internal/threadstore"

	"github.com/nats-io/nats.go"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

var (
	errOwnershipConflict = errors.New("thread is owned by another worker")
	errCommandPrecond    = errors.New("command precondition failed")
	errRemotePermanent   = errors.New("openai returned a permanent error")
	errUnsupportedKind   = errors.New("unsupported command kind")
)

const (
	maxTransientCommandDeliveries = 5
	transientCommandRetryBase     = 2 * time.Second
	transientCommandRetryMax      = 30 * time.Second
)

type threadActorConfig struct {
	ThreadID       string
	WorkerID       string
	Logger         *slog.Logger
	Store          actorStore
	History        threadHistoryStore
	ThreadDocs     threadDocumentStore
	DocRuntime     docRuntimeContextClient
	DocStore       docActorDocStore
	PreparedInputs docActorPreparedInputClient
	Blob           *blobstore.LocalStore
	OpenAIConfig   openaiws.Config
	Publish        func(ctx context.Context, subject string, cmd agentcmd.Command) error
	PublishEvent   func(ctx context.Context, threadID string, socketGeneration uint64, key string, eventType string, raw json.RawMessage) error
	SessionFactory func() *openaiws.Session
}

type actorStore interface {
	CreateThreadIfAbsent(ctx context.Context, meta threadstore.ThreadMeta) error
	LoadThread(ctx context.Context, threadID string) (threadstore.ThreadMeta, error)
	LoadLatestCompletedDocumentQueryLineage(ctx context.Context, parentThreadID, documentID string) (threadstore.DocumentQueryLineage, error)
	SaveThread(ctx context.Context, meta threadstore.ThreadMeta) error
	CommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error)
	MarkCommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error)
	ClaimOwnership(ctx context.Context, threadID, workerID string, leaseUntil time.Time) (threadstore.ClaimResult, error)
	RenewOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64, leaseUntil time.Time) (bool, error)
	RotateOwnership(ctx context.Context, threadID, workerID string, currentGeneration uint64, leaseUntil, socketExpiresAt time.Time) (uint64, bool, error)
	ReleaseOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64) error
	AppendItem(ctx context.Context, entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error)
	SaveResponseRaw(ctx context.Context, threadID, responseID string, payload json.RawMessage) error
	ListItems(ctx context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.ItemRecord, error)
	CreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta, childThreadIDs []string) error
	LoadSpawnGroup(ctx context.Context, spawnGroupID string) (threadstore.SpawnGroupMeta, error)
	SaveSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) error
	ListSpawnResults(ctx context.Context, spawnGroupID string) ([]threadstore.SpawnChildResult, error)
	UpsertSpawnResult(ctx context.Context, spawnGroupID string, result threadstore.SpawnChildResult) (bool, []threadstore.SpawnChildResult, error)
}

type threadHistoryStore interface {
	SaveResponseCreateCheckpoint(ctx context.Context, threadID, checkpointID string, payload json.RawMessage) error
	LoadLatestResponseCreateCheckpoint(ctx context.Context, threadID string) (json.RawMessage, error)
	AppendEvent(ctx context.Context, entry threadstore.EventLogEntry, eventID string) error
}

type threadDocumentStore interface {
	ListDocuments(ctx context.Context, threadID string, limit int64) ([]docstore.Document, error)
	FilterAttached(ctx context.Context, threadID string, documentIDs []string) ([]string, error)
}

type docActorDocStore interface {
	Get(ctx context.Context, id string) (docstore.Document, error)
	UpdateBaseLineage(ctx context.Context, id, baseResponseID, baseModel string) error
}

type docActorPreparedInputClient interface {
	PrepareInput(ctx context.Context, req doccmd.PrepareInputRequest) (doccmd.PrepareInputResponse, error)
}

type threadActor struct {
	threadID       string
	workerID       string
	logger         *slog.Logger
	store          actorStore
	history        threadHistoryStore
	threadDocs     threadDocumentStore
	docRuntime     docRuntimeContextClient
	docStore       docActorDocStore
	preparedInputs docActorPreparedInputClient
	blob           *blobstore.LocalStore
	cfg            openaiws.Config
	publish        func(ctx context.Context, subject string, cmd agentcmd.Command) error
	publishEvent   func(ctx context.Context, threadID string, socketGeneration uint64, key string, eventType string, raw json.RawMessage) error

	sessionFactory func() *openaiws.Session

	ctx    context.Context
	cancel context.CancelFunc

	commands chan queuedCommand
	done     chan struct{}

	mu          sync.Mutex
	session     *openaiws.Session
	leaseCancel context.CancelFunc
	idleCancel  context.CancelFunc
	idleDone    chan struct{}
	meta        threadstore.ThreadMeta
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
		threadID:       cfg.ThreadID,
		workerID:       cfg.WorkerID,
		logger:         cfg.Logger,
		store:          cfg.Store,
		history:        cfg.History,
		threadDocs:     cfg.ThreadDocs,
		docRuntime:     cfg.DocRuntime,
		docStore:       cfg.DocStore,
		preparedInputs: cfg.PreparedInputs,
		blob:           cfg.Blob,
		cfg:            cfg.OpenAIConfig,
		publish:        cfg.Publish,
		publishEvent:   cfg.PublishEvent,
		sessionFactory: cfg.SessionFactory,
		ctx:            ctx,
		cancel:         cancel,
		commands:       make(chan queuedCommand, commandQueueSize),
		done:           make(chan struct{}),
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
	meta := a.meta
	a.session = nil
	a.mu.Unlock()

	if idleDone != nil {
		<-idleDone
	}

	if session != nil {
		errs = append(errs, session.Close())
	}

	if meta.ID != "" && meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		errs = append(errs, a.store.ReleaseOwnership(context.Background(), meta.ID, a.workerID, meta.SocketGeneration))
	}

	return errors.Join(errs...)
}

func (a *threadActor) run() {
	defer close(a.done)

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
		}
	}
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
		"cmd_id", queued.cmd.CmdID,
		"kind", queued.cmd.Kind,
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
		"cmd_id", queued.cmd.CmdID,
		"kind", queued.cmd.Kind,
	)

	return queued.msg.Ack()
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

func (a *threadActor) failThreadAfterRetryExhaustion(threadID string) error {
	meta, err := a.store.LoadThread(a.ctx, threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return nil
		}
		return err
	}

	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession()

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
		meta.OwnerWorkerID = ""
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

func (a *threadActor) processCommand(cmd agentcmd.Command) error {
	switch cmd.Kind {
	case agentcmd.KindThreadStart:
		return a.handleStart(cmd)
	case agentcmd.KindThreadResume:
		return a.handleResume(cmd)
	case agentcmd.KindThreadSubmitToolOutput:
		return a.handleSubmitToolOutput(cmd)
	case agentcmd.KindThreadChildCompleted:
		return a.handleChildResult(cmd, "completed")
	case agentcmd.KindThreadChildFailed:
		return a.handleChildResult(cmd, "failed")
	case agentcmd.KindThreadAdopt:
		return a.handleAdopt(cmd)
	case agentcmd.KindThreadRotateSocket:
		return a.handleRotateSocket(cmd)
	case agentcmd.KindThreadReconcile:
		return a.handleReconcile(cmd)
	case agentcmd.KindThreadCancel:
		return a.handleCancel(cmd)
	case agentcmd.KindThreadDisconnectSocket:
		return a.handleDisconnectSocket(cmd)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedKind, cmd.Kind)
	}
}

func (a *threadActor) handleStart(cmd agentcmd.Command) error {
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
	body.Include, err = agentcmd.NormalizeInclude(body.Include)
	if err != nil {
		return fmt.Errorf("normalize thread.start include: %w", err)
	}
	body.Tools, err = agentcmd.NormalizeTools(body.Tools)
	if err != nil {
		return fmt.Errorf("normalize thread.start tools: %w", err)
	}
	body.ToolChoice, err = agentcmd.NormalizeToolChoice(body.ToolChoice)
	if err != nil {
		return fmt.Errorf("normalize thread.start tool_choice: %w", err)
	}
	body.Reasoning, err = agentcmd.NormalizeReasoning(body.Reasoning)
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
	if strings.TrimSpace(meta.ParentThreadID) != "" {
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

func (a *threadActor) handleResume(cmd agentcmd.Command) error {
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
		return fmt.Errorf("%w: thread %s has no last_response_id for resume", errCommandPrecond, meta.ID)
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

func (a *threadActor) handleSubmitToolOutput(cmd agentcmd.Command) error {
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
		return fmt.Errorf("%w: thread %s has no last_response_id for tool output", errCommandPrecond, meta.ID)
	}

	if meta.Status != threadstore.ThreadStatusWaitingTool && meta.Status != threadstore.ThreadStatusReady {
		return fmt.Errorf("thread %s is not ready for tool output, current status=%s", meta.ID, meta.Status)
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

func (a *threadActor) handleChildResult(cmd agentcmd.Command, fallbackStatus string) error {
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
		return fmt.Errorf("%w: thread %s is not waiting on children, current status=%s", errCommandPrecond, meta.ID, meta.Status)
	}

	spawnGroupID := body.SpawnGroupID
	if meta.ActiveSpawnGroupID != "" && meta.ActiveSpawnGroupID != spawnGroupID {
		return fmt.Errorf("thread %s active spawn group mismatch: meta=%s cmd=%s", meta.ID, meta.ActiveSpawnGroupID, spawnGroupID)
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
		ChildThreadID:   body.ChildThreadID,
		Status:          status,
		ChildResponseID: body.ChildResponseID,
		AssistantText:   body.AssistantText,
		ResultRef:       body.ResultRef,
		SummaryRef:      body.SummaryRef,
		ErrorRef:        body.ErrorRef,
		UpdatedAt:       time.Now().UTC(),
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
			"completed_children", spawn.Completed,
			"failed_children", spawn.Failed,
			"cancelled_children", spawn.Cancelled,
			"expected_children", spawn.Expected,
		}, meta)...,
	)

	if spawn.AggregateSubmittedAt.IsZero() && spawn.Completed+spawn.Failed+spawn.Cancelled >= spawn.Expected {
		outputItem, err := aggregateSpawnOutputItem(spawn, results)
		if err != nil {
			return err
		}

		spawn.AggregateSubmittedAt = time.Now().UTC()
		spawn.AggregateCmdID = cmd.CmdID
		if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
			return err
		}

		inputItems, err := wrapRawItemAsArray(outputItem)
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

func (a *threadActor) handleDocumentWarmupChildResult(parentMeta threadstore.ThreadMeta, spawn threadstore.SpawnGroupMeta, body agentcmd.ChildResultBody, status string) (bool, error) {
	if status != "completed" || strings.TrimSpace(body.ChildThreadID) == "" {
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
		return false, fmt.Errorf("decode child metadata for %s: %w", body.ChildThreadID, err)
	}
	if childMetadata["spawn_mode"] != "document_warmup" {
		return false, nil
	}
	if strings.TrimSpace(body.ChildResponseID) == "" {
		return false, fmt.Errorf("document warmup child %s completed without child_response_id", body.ChildThreadID)
	}
	if a.docStore == nil {
		return false, fmt.Errorf("document store not available")
	}

	documentID := strings.TrimSpace(childMetadata["document_id"])
	if documentID == "" {
		return false, fmt.Errorf("document warmup child %s missing document_id metadata", body.ChildThreadID)
	}
	documentTask := strings.TrimSpace(childMetadata["document_task"])
	if documentTask == "" {
		return false, fmt.Errorf("document warmup child %s missing document_task metadata", body.ChildThreadID)
	}
	parentCallID := strings.TrimSpace(childMeta.ParentCallID)
	if parentCallID == "" {
		parentCallID = spawn.ParentCallID
	}
	model := defaultString(strings.TrimSpace(childMeta.Model), strings.TrimSpace(parentMeta.Model))

	if err := a.docStore.UpdateBaseLineage(a.ctx, documentID, body.ChildResponseID, model); err != nil {
		return false, fmt.Errorf("update document base lineage for %s: %w", documentID, err)
	}

	queryThreadID, startCmd, err := a.buildDocumentChildStartCommand(
		parentMeta,
		spawn.ID,
		parentCallID,
		documentID,
		childMetadata["document_name"],
		model,
		"query",
		body.ChildResponseID,
		"",
		documentTask,
		body.ChildThreadID,
	)
	if err != nil {
		return false, err
	}

	if err := a.publish(a.ctx, agentcmd.DispatchStartSubject, startCmd); err != nil {
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
			"model", model,
		}, parentMeta)...,
	)
	return true, nil
}

func (a *threadActor) handleAdopt(cmd agentcmd.Command) error {
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

func (a *threadActor) handleRotateSocket(cmd agentcmd.Command) error {
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
		return fmt.Errorf("%w: thread %s status %s is not safe for rotation", errCommandPrecond, meta.ID, meta.Status)
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
		_ = freshSession.Close()
		a.startLeaseLoop(meta)
		a.startIdleLoop(meta)
		return err
	}
	if !rotated {
		_ = freshSession.Close()
		a.handleLeaseLoss(meta.SocketGeneration)
		return errOwnershipConflict
	}

	oldSession := a.swapSession(freshSession)
	if oldSession != nil {
		if err := oldSession.Close(); err != nil {
			a.logger.Warn("failed to close rotated socket", "error", err)
		}
	}

	meta.OwnerWorkerID = a.workerID
	meta.SocketGeneration = newGeneration
	meta.SocketExpiresAt = now.Add(socketExpiryTTL)
	meta.UpdatedAt = now
	a.setMeta(meta)
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

func (a *threadActor) handleReconcile(cmd agentcmd.Command) error {
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

func (a *threadActor) recoverThread(meta threadstore.ThreadMeta, cmdID string, forceReconcile bool) error {
	needsBarrierRecovery := meta.ActiveSpawnGroupID != "" && meta.ActiveResponseID == "" &&
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

func (a *threadActor) handleCancel(cmd agentcmd.Command) error {
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
	a.session = nil
	a.mu.Unlock()

	if idleDone != nil {
		<-idleDone
	}

	if session != nil {
		if err := session.Close(); err != nil {
			return err
		}
	}
	return a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration)
}

func (a *threadActor) handleDisconnectSocket(cmd agentcmd.Command) error {
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
	a.resetSession()

	return a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration)
}

func (a *threadActor) recoverWaitingChildren(meta threadstore.ThreadMeta, cmdID string) error {
	if meta.ActiveSpawnGroupID == "" {
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

	outputItem, err := aggregateSpawnOutputItem(spawn, results)
	if err != nil {
		return err
	}

	inputItems, err := wrapRawItemAsArray(outputItem)
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

func (a *threadActor) reconcileFromCheckpoint(meta threadstore.ThreadMeta, cmdID string) error {
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
	if err := a.finalizeThreadResponseCreatePayload(meta.ID, payload); err != nil {
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
	a.resetSession()

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
		meta.OwnerWorkerID = ""
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

	a.logger.Info("openai websocket session connected",
		appendThreadGraphAttrs([]any{
			"socket_generation", a.currentSocketGeneration(),
			"session_connect_count", newSession.Snapshot().SocketGeneration,
		}, a.currentMeta())...,
	)

	oldSession := a.swapSession(newSession)
	if oldSession != nil {
		if err := oldSession.Close(); err != nil {
			a.logger.Warn("failed to close previous socket", "error", err)
		}
	}
	return nil
}

func (a *threadActor) openFreshSession() (*openaiws.Session, error) {
	session := a.sessionFactory()
	if err := session.Connect(a.ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func (a *threadActor) swapSession(next *openaiws.Session) *openaiws.Session {
	a.mu.Lock()
	defer a.mu.Unlock()

	prev := a.session
	a.session = next
	return prev
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
			renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			renewed, err := a.store.RenewOwnership(renewCtx, meta.ID, a.workerID, meta.SocketGeneration, time.Now().UTC().Add(workerLeaseTTL))
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
	a.session = nil
	idleDone := a.idleDone
	a.mu.Unlock()

	if idleDone != nil {
		<-idleDone
	}

	if session != nil {
		_ = session.Close()
	}

	a.cancel()
}

func (a *threadActor) sendAndStream(meta threadstore.ThreadMeta, eventID string, payload map[string]any, trigger string) error {
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
		if err := a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, "client-response-create", threadevents.EventTypeClientResponse, rawEvent); err != nil {
			return err
		}
	}

	return a.streamUntilTerminal(meta)
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
	if rootThreadID := strings.TrimSpace(defaultRootThreadID(meta)); rootThreadID != "" && !hasLogAttrKey(attrs, "root_thread_id") {
		attrs = append(attrs, "root_thread_id", rootThreadID)
	}
	if parentThreadID := strings.TrimSpace(meta.ParentThreadID); parentThreadID != "" && !hasLogAttrKey(attrs, "parent_thread_id") {
		attrs = append(attrs, "parent_thread_id", parentThreadID)
	}
	if parentCallID := strings.TrimSpace(meta.ParentCallID); parentCallID != "" && !hasLogAttrKey(attrs, "parent_call_id") {
		attrs = append(attrs, "parent_call_id", parentCallID)
	}
	if !hasLogAttrKey(attrs, "depth") {
		attrs = append(attrs, "depth", meta.Depth)
	}
	if spawnGroupID := strings.TrimSpace(meta.ActiveSpawnGroupID); spawnGroupID != "" && !hasLogAttrKey(attrs, "spawn_group_id") {
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

func (a *threadActor) continueWithInputItems(meta threadstore.ThreadMeta, cmdID string, inputItems json.RawMessage, trigger string) error {
	return a.continueWithPreparedInput(meta, cmdID, inputItems, "", trigger)
}

func (a *threadActor) continueWithPreparedInput(meta threadstore.ThreadMeta, cmdID string, inputItems json.RawMessage, preparedInputRef string, trigger string) error {
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

func (a *threadActor) applyDocumentRuntimeContext(threadID string, payload map[string]any) error {
	if a.threadDocs == nil {
		return nil
	}

	documents, err := a.threadDocs.ListDocuments(a.ctx, threadID, 200)
	if err != nil {
		return fmt.Errorf("list attached documents for runtime context: %w", err)
	}
	if len(documents) == 0 {
		return nil
	}
	if a.docRuntime == nil {
		return a.applyLocalDocumentRuntimeContext(payload, documents)
	}

	rawTools, err := marshalRuntimeContextTools(payload["tools"])
	if err != nil {
		return err
	}

	requestID := fmt.Sprintf("docctx_%s_%d", threadID, time.Now().UTC().UnixNano())
	resp, err := a.docRuntime.RuntimeContext(a.ctx, doccmd.RuntimeContextRequest{
		RequestID:    requestID,
		ThreadID:     threadID,
		Instructions: stringValue(payload["instructions"]),
		Tools:        rawTools,
	})
	if err != nil {
		return a.fallbackDocumentRuntimeContext(threadID, payload, documents, fmt.Errorf("load document runtime context: %w", err))
	}
	if resp.RequestID != requestID {
		return a.fallbackDocumentRuntimeContext(threadID, payload, documents, fmt.Errorf("document runtime context request id mismatch: got %q want %q", resp.RequestID, requestID))
	}
	if strings.TrimSpace(resp.Status) != doccmd.PrepareStatusOK {
		if strings.TrimSpace(resp.Error) == "" {
			resp.Error = fmt.Sprintf("runtime context returned status %q", resp.Status)
		}
		return a.fallbackDocumentRuntimeContext(threadID, payload, documents, fmt.Errorf("document runtime context: %s", resp.Error))
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
		return a.fallbackDocumentRuntimeContext(threadID, payload, documents, fmt.Errorf("decode document runtime tools: %w", err))
	}
	payload["tools"] = decoded
	return nil
}

func (a *threadActor) fallbackDocumentRuntimeContext(_ string, payload map[string]any, documents []docstore.Document, cause error) error {
	if a.logger != nil {
		a.logger.Warn("document runtime context unavailable, falling back to local augmentation",
			"document_count", len(documents),
			"error", cause,
		)
	}
	return a.applyLocalDocumentRuntimeContext(payload, documents)
}

func (a *threadActor) applyLocalDocumentRuntimeContext(payload map[string]any, documents []docstore.Document) error {
	instructions := appendAvailableDocumentsBlockLocal(stringValue(payload["instructions"]), documents)
	if strings.TrimSpace(instructions) == "" {
		delete(payload, "instructions")
	} else {
		payload["instructions"] = instructions
	}

	rawTools, err := marshalRuntimeContextTools(payload["tools"])
	if err != nil {
		return err
	}
	rawTools, err = appendQueryAttachedDocumentsToolLocal(rawTools)
	if err != nil {
		return err
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

func appendAvailableDocumentsBlockLocal(base string, documents []docstore.Document) string {
	block := formatAvailableDocumentsBlockLocal(documents)
	if block == "" {
		return base
	}

	trimmedBase := strings.TrimRight(base, "\n")
	if strings.TrimSpace(trimmedBase) == "" {
		return block
	}

	return trimmedBase + "\n\n" + block
}

func formatAvailableDocumentsBlockLocal(documents []docstore.Document) string {
	var builder strings.Builder
	count := 0

	for _, document := range documents {
		id := strings.TrimSpace(document.ID)
		if id == "" {
			continue
		}

		name := strings.TrimSpace(document.Filename)
		if name == "" {
			name = id
		}

		if count == 0 {
			builder.WriteString("<available_documents>\n")
		}
		builder.WriteString(`<document id="`)
		builder.WriteString(escapePromptAttributeLocal(id))
		builder.WriteString(`" name="`)
		builder.WriteString(escapePromptAttributeLocal(name))
		builder.WriteString(`" />`)
		builder.WriteByte('\n')
		count++
	}

	if count == 0 {
		return ""
	}

	builder.WriteString("</available_documents>")
	return builder.String()
}

func escapePromptAttributeLocal(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
		"\n", "&#10;",
		"\r", "&#13;",
		"\t", "&#9;",
	)
	return replacer.Replace(value)
}

func appendQueryAttachedDocumentsToolLocal(raw json.RawMessage) (json.RawMessage, error) {
	var tools []map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode local document runtime tools: %w", err)
		}
	}

	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == doccmd.ToolNameQueryAttachedDocuments {
			return raw, nil
		}
	}

	tools = append(tools, doccmd.QueryAttachedDocumentsToolDefinition())
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("marshal local document runtime tools: %w", err)
	}
	return encoded, nil
}

type docQueryRequest struct {
	DocumentIDs []string `json:"document_ids"`
	Task        string   `json:"task"`
}

func decodeDocQueryRequest(arguments string) (docQueryRequest, error) {
	var req docQueryRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return docQueryRequest{}, fmt.Errorf("decode %s arguments: %w", doccmd.ToolNameQueryAttachedDocuments, err)
	}
	if len(req.DocumentIDs) == 0 {
		return docQueryRequest{}, fmt.Errorf("%s requires at least one document_id", doccmd.ToolNameQueryAttachedDocuments)
	}
	if strings.TrimSpace(req.Task) == "" {
		return docQueryRequest{}, fmt.Errorf("%s requires a non-empty task", doccmd.ToolNameQueryAttachedDocuments)
	}
	return req, nil
}

func (a *threadActor) streamUntilTerminal(meta threadstore.ThreadMeta) error {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return openaiws.ErrNotConnected
	}

	a.logger.Info("streaming response events from openai",
		appendThreadGraphAttrs([]any{
			"socket_generation", meta.SocketGeneration,
		}, meta)...,
	)

	waitingTool := false
	var pendingSpawn *spawnRequest
	var pendingSpawnCallID string
	var pendingDocQuery *docQueryRequest
	var pendingDocQueryCallID string
	prevStatus := meta.Status
	eventCount := 0
	deltaLogs := map[openaiws.EventType]*deltaLogState{}

	for {
		event, err := session.Receive(a.ctx)
		if err != nil {
			a.logger.Error("stream receive error",
				"events_received", eventCount,
				"error", err,
			)
			return err
		}
		eventCount++

		if strings.HasSuffix(string(event.Type), ".delta") {
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
				a.logger.Info("received openai event", "event_type", event.Type)
			} else {
				state.lastRaw = string(event.Raw)
				state.suppressedAny = true
			}
		} else if event.Type == openaiws.EventTypeResponseOutputItemAdded {
			a.flushDeltaLogForDoneEvent(deltaLogs, event.Type)
			a.logOutputItemEvent(event)
		} else if event.Type == openaiws.EventTypeResponseOutputItemDone {
			a.flushDeltaLogForDoneEvent(deltaLogs, event.Type)
			a.logOutputItemEvent(event)
		} else {
			a.flushDeltaLogForDoneEvent(deltaLogs, event.Type)
			a.logger.Info("received openai event", "event_type", event.Type)
		}

		responseID := event.ResolvedResponseID()
		if !strings.HasSuffix(string(event.Type), ".delta") {
			if a.history == nil {
				return fmt.Errorf("thread history store is not configured")
			}
			if err := a.history.AppendEvent(a.ctx, threadstore.EventLogEntry{
				ThreadID:         meta.ID,
				SocketGeneration: meta.SocketGeneration,
				EventType:        string(event.Type),
				ResponseID:       responseID,
				PayloadJSON:      string(event.Raw),
				CreatedAt:        time.Now().UTC(),
			}, ""); err != nil {
				return err
			}
		}

		if a.publishEvent != nil {
			if err := a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, fmt.Sprintf("event-%d", eventCount), string(event.Type), event.Raw); err != nil {
				return err
			}
		}

		if responsePayload := event.ResponsePayload(); len(responsePayload) > 0 {
			if err := a.store.SaveResponseRaw(a.ctx, meta.ID, responseID, responsePayload); err != nil {
				return err
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
					if err == nil && call.Name == "spawn_subagents" {
						req, err := decodeSpawnRequest(call.Arguments, meta)
						if err != nil {
							return err
						}
						pendingSpawn = &req
						pendingSpawnCallID = call.CallID
					} else if err == nil && call.Name == doccmd.ToolNameQueryAttachedDocuments {
						req, err := decodeDocQueryRequest(call.Arguments)
						if err != nil {
							a.logger.Warn("invalid document query arguments, falling back to waiting_tool",
								"call_id", call.CallID,
								"error", err,
							)
							waitingTool = true
						} else {
							pendingDocQuery = &req
							pendingDocQueryCallID = call.CallID
						}
					} else {
						waitingTool = true
					}
				}
				a.logger.Info("output item received",
					"item_type", itemType,
					"response_id", responseID,
				)
				if _, err := a.appendItem(threadstore.ItemLogEntry{
					ThreadID:    meta.ID,
					ResponseID:  responseID,
					ItemType:    itemType,
					Direction:   "output",
					PayloadJSON: string(itemRaw),
					CreatedAt:   time.Now().UTC(),
				}); err != nil {
					return err
				}
			}
		case openaiws.EventTypeResponseCompleted:
			if responseID != "" {
				meta.LastResponseID = responseID
			}
			meta.ActiveResponseID = ""
			if pendingSpawn != nil {
				spawnGroupID, err := a.startSpawnGroup(meta, pendingSpawnCallID, *pendingSpawn)
				if err != nil {
					return err
				}
				meta.ActiveSpawnGroupID = spawnGroupID
				meta.Status = threadstore.ThreadStatusWaitingChildren
			} else if pendingDocQuery != nil && !waitingTool {
				spawnGroupID, err := a.startDocumentQueryGroup(meta, pendingDocQueryCallID, *pendingDocQuery)
				if err != nil {
					return err
				}
				meta.ActiveSpawnGroupID = spawnGroupID
				meta.Status = threadstore.ThreadStatusWaitingChildren
			} else if waitingTool {
				meta.Status = threadstore.ThreadStatusWaitingTool
			} else {
				if meta.ParentThreadID != "" {
					meta.Status = threadstore.ThreadStatusCompleted
				} else {
					meta.Status = threadstore.ThreadStatusReady
				}
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
			return err
		}

		if event.Type == openaiws.EventTypeError {
			a.resetSession()
			if event.Error != nil && event.Error.Message != "" {
				return fmt.Errorf("%w: %s", errRemotePermanent, event.Error.Message)
			}
			return fmt.Errorf("%w: openai error event received", errRemotePermanent)
		}

		if event.Type.IsTerminal() {
			a.flushAllDeltaLogs(deltaLogs)
			a.logger.Info("stream completed",
				appendThreadGraphAttrs([]any{
					"final_status", meta.Status,
					"last_response_id", meta.LastResponseID,
					"events_received", eventCount,
				}, meta)...,
			)
			if shouldPublishChildTerminal(meta.Status, meta) {
				if err := a.publishChildTerminal(meta); err != nil {
					return err
				}
			}
			if shouldReleaseTerminalChildResources(meta) {
				if err := a.releaseTerminalChildResources(&meta); err != nil {
					return err
				}
			}
			if statusSupportsIdleSocket(meta.Status) {
				a.startIdleLoop(meta)
			}
			return nil
		}
	}
}

func (a *threadActor) flushDeltaLogForDoneEvent(deltaLogs map[openaiws.EventType]*deltaLogState, eventType openaiws.EventType) {
	if !strings.HasSuffix(string(eventType), ".done") {
		return
	}

	deltaType := openaiws.EventType(strings.TrimSuffix(string(eventType), ".done") + ".delta")
	a.flushDeltaLog(deltaLogs, deltaType)
}

func (a *threadActor) flushAllDeltaLogs(deltaLogs map[openaiws.EventType]*deltaLogState) {
	for eventType := range deltaLogs {
		a.flushDeltaLog(deltaLogs, eventType)
	}
}

func (a *threadActor) flushDeltaLog(deltaLogs map[openaiws.EventType]*deltaLogState, eventType openaiws.EventType) {
	state := deltaLogs[eventType]
	if state == nil {
		return
	}
	delete(deltaLogs, eventType)

	if !state.suppressedAny || state.lastRaw == "" || state.lastRaw == state.firstRaw {
		return
	}

	a.logger.Info("received openai event", "event_type", eventType)
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

func (a *threadActor) appendInputItems(threadID, responseID string, itemsRaw json.RawMessage) error {
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

func (a *threadActor) appendItem(entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error) {
	item, err := a.store.AppendItem(a.ctx, entry)
	if err != nil {
		return threadstore.ItemRecord{}, err
	}
	a.publishThreadItem(item)
	return item, nil
}

func (a *threadActor) saveThreadMeta(meta threadstore.ThreadMeta) error {
	prev := a.currentMeta()
	if err := a.store.SaveThread(a.ctx, meta); err != nil {
		return err
	}
	a.setMeta(meta)
	if shouldPublishThreadSnapshot(prev, meta) {
		a.publishThreadSnapshot(meta)
	}
	return nil
}

func shouldPublishThreadSnapshot(prev, next threadstore.ThreadMeta) bool {
	if prev.ID == "" {
		return true
	}
	return prev.ID != next.ID ||
		prev.Status != next.Status ||
		prev.Model != next.Model ||
		prev.LastResponseID != next.LastResponseID ||
		prev.ActiveResponseID != next.ActiveResponseID ||
		prev.ActiveSpawnGroupID != next.ActiveSpawnGroupID
}

func (a *threadActor) publishThreadSnapshot(meta threadstore.ThreadMeta) {
	if a.publishEvent == nil {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"id":                    meta.ID,
		"status":                meta.Status,
		"model":                 meta.Model,
		"last_response_id":      meta.LastResponseID,
		"active_response_id":    meta.ActiveResponseID,
		"active_spawn_group_id": meta.ActiveSpawnGroupID,
		"updated_at":            meta.UpdatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		a.logger.Warn("failed to marshal thread snapshot event", "error", err)
		return
	}

	if err := a.publishEvent(
		a.ctx,
		meta.ID,
		meta.SocketGeneration,
		fmt.Sprintf("snapshot-%d", meta.UpdatedAt.UnixNano()),
		threadevents.EventTypeThreadSnapshot,
		payload,
	); err != nil {
		a.logger.Warn("failed to publish thread snapshot event", "error", err)
	}
}

func (a *threadActor) publishThreadItem(item threadstore.ItemRecord) {
	if a.publishEvent == nil {
		return
	}

	payload, err := json.Marshal(presentThreadItem(item))
	if err != nil {
		a.logger.Warn("failed to marshal thread item event", "seq", item.Seq, "error", err)
		return
	}

	if err := a.publishEvent(
		a.ctx,
		a.threadID,
		a.currentSocketGeneration(),
		fmt.Sprintf("item-%d", item.Seq),
		threadevents.EventTypeThreadItem,
		payload,
	); err != nil {
		a.logger.Warn("failed to publish thread item event", "seq", item.Seq, "error", err)
	}
}

func presentThreadItem(item threadstore.ItemRecord) map[string]any {
	response := map[string]any{
		"cursor":      strconv.FormatInt(item.Seq, 10),
		"seq":         item.Seq,
		"response_id": item.ResponseID,
		"item_type":   item.ItemType,
		"direction":   item.Direction,
		"created_at":  item.CreatedAt.UTC().Format(time.RFC3339),
	}
	if decoded, err := decodeStreamRawJSON(item.Payload); err == nil && decoded != nil {
		response["payload"] = decoded
	}
	return response
}

func decodeStreamRawJSON(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return string(trimmed), nil
	}
	return decoded, nil
}

func (a *threadActor) loadLatestAssistantText(threadID string) (string, error) {
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
	a.resetSession()

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
	}

	meta.OwnerWorkerID = ""
	meta.SocketExpiresAt = time.Time{}
	a.setMeta(*meta)

	a.logger.Info("released terminal thread socket",
		appendThreadGraphAttrs([]any{
			"socket_generation", meta.SocketGeneration,
		}, *meta)...,
	)

	return nil
}

func (a *threadActor) currentSocketGeneration() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.meta.SocketGeneration
}

func (a *threadActor) resetSession() {
	a.mu.Lock()
	session := a.session
	a.session = nil
	a.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
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

func (a *threadActor) logOutputItemEvent(event openaiws.ServerEvent) {
	attrs := []any{"event_type", outputItemLogEventType(event)}
	if sequenceNumber, ok := outputItemEventSequenceNumber(event); ok {
		attrs = append(attrs, "event_sequence_number", sequenceNumber)
	}
	a.logger.Info("received openai event", attrs...)
}

func outputItemLogEventType(event openaiws.ServerEvent) string {
	if event.Type != openaiws.EventTypeResponseOutputItemAdded && event.Type != openaiws.EventTypeResponseOutputItemDone {
		return string(event.Type)
	}

	itemRaw := event.Field("item")
	if len(itemRaw) == 0 && len(event.Raw) > 0 {
		var payload struct {
			Item json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(event.Raw, &payload); err == nil {
			itemRaw = payload.Item
		}
	}

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
		children = append(children, child)
	}

	outputPayload, err := json.Marshal(map[string]any{
		"spawn_group_id": spawn.ID,
		"children":       children,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal aggregate spawn output payload: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"type":    "function_call_output",
		"call_id": spawn.ParentCallID,
		"output":  string(outputPayload),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal aggregate spawn output item: %w", err)
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
	ThreadID     string          `json:"thread_id,omitempty"`
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
		return spawnRequest{}, fmt.Errorf("decode spawn_subagents arguments: %w", err)
	}

	if len(request.Children) == 0 {
		return spawnRequest{}, fmt.Errorf("spawn_subagents requires at least one child")
	}

	if parentMeta.Depth >= 1 {
		return spawnRequest{}, fmt.Errorf("spawn_subagents depth limit reached for thread %s", parentMeta.ID)
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

func (a *threadActor) startDocumentQueryGroup(parentMeta threadstore.ThreadMeta, parentCallID string, req docQueryRequest) (string, error) {
	if a.threadDocs == nil {
		return "", fmt.Errorf("document store not available")
	}
	req.DocumentIDs = uniqueStringsPreserveOrder(req.DocumentIDs)

	attached, err := a.threadDocs.FilterAttached(a.ctx, parentMeta.ID, req.DocumentIDs)
	if err != nil {
		return "", fmt.Errorf("validate attached documents: %w", err)
	}

	attachedSet := make(map[string]bool, len(attached))
	for _, id := range attached {
		attachedSet[id] = true
	}
	var missing []string
	for _, id := range req.DocumentIDs {
		if !attachedSet[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("documents not attached to thread: %v", missing)
	}

	spawnGroupID := stableDocumentSpawnGroupID(parentMeta.ID, parentCallID)
	childThreadIDs := make([]string, 0, len(req.DocumentIDs))
	childCommands := make([]agentcmd.Command, 0, len(req.DocumentIDs))

	for _, docID := range req.DocumentIDs {
		doc, err := a.docStore.Get(a.ctx, docID)
		if err != nil {
			return "", fmt.Errorf("load document %s: %w", docID, err)
		}

		model := doc.QueryModel
		if model == "" {
			model = parentMeta.Model
		}

		previousResponseID := ""
		lineageSource := "warmup"
		lineage, err := a.store.LoadLatestCompletedDocumentQueryLineage(a.ctx, parentMeta.ID, docID)
		if err != nil && !errors.Is(err, threadstore.ErrThreadNotFound) {
			return "", fmt.Errorf("get latest completed document child lineage for %s: %w", docID, err)
		}
		if err == nil {
			previousResponseID = lineage.ResponseID
			lineageSource = "thread_local"
			if strings.TrimSpace(lineage.Model) != "" {
				model = lineage.Model
			}
		}
		if previousResponseID == "" && doc.BaseResponseID != "" && doc.BaseModel == model {
			previousResponseID = doc.BaseResponseID
			lineageSource = "document_base"
		}

		phase := "query"
		var preparedInputRef string
		if previousResponseID == "" {
			phase = "warmup"
			resp, err := a.preparedInputs.PrepareInput(a.ctx, doccmd.PrepareInputRequest{
				RequestID:  stableDocumentPreparedInputID(parentMeta.ID, parentCallID, docID, phase),
				Kind:       doccmd.PrepareKindWarmup,
				ThreadID:   parentMeta.ID,
				DocumentID: docID,
			})
			if err != nil {
				return "", fmt.Errorf("prepare warmup input for document %s: %w", docID, err)
			}
			if resp.Status != doccmd.PrepareStatusOK {
				return "", fmt.Errorf("prepare warmup input for document %s failed: %s", docID, resp.Error)
			}
			preparedInputRef = resp.PreparedInputRef
		}

		threadID, startCmd, err := a.buildDocumentChildStartCommand(parentMeta, spawnGroupID, parentCallID, docID, doc.Filename, model, phase, previousResponseID, preparedInputRef, req.Task, "")
		if err != nil {
			return "", err
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
				"document_id", docID,
				"document_name", doc.Filename,
				"phase", phase,
				"model", model,
				"lineage_source", lineageSource,
				"has_previous_response_id", previousResponseID != "",
			}, parentMeta)...,
		)
	}

	if err := a.store.CreateSpawnGroup(a.ctx, threadstore.SpawnGroupMeta{
		ID:             spawnGroupID,
		ParentThreadID: parentMeta.ID,
		ParentCallID:   parentCallID,
		Expected:       len(childCommands),
		Status:         threadstore.SpawnGroupStatusWaiting,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}, childThreadIDs); err != nil {
		return "", err
	}

	a.logger.Info("opened child barrier",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawnGroupID,
			"child_source", "document_query",
			"expected_children", len(childCommands),
		}, parentMeta)...,
	)

	for _, cmd := range childCommands {
		if err := a.publish(a.ctx, agentcmd.DispatchStartSubject, cmd); err != nil {
			return "", err
		}
	}

	return spawnGroupID, nil
}

func (a *threadActor) buildDocumentChildStartCommand(parentMeta threadstore.ThreadMeta, spawnGroupID, parentCallID, documentID, documentName, model, phase, previousResponseID, preparedInputRef, task, bootstrapChildThreadID string) (string, agentcmd.Command, error) {
	threadID := stableDocumentChildThreadID(parentMeta.ID, parentCallID, documentID, phase)
	cmdID := stableDocumentChildCmdID(parentMeta.ID, parentCallID, documentID, phase)

	startBody := map[string]any{
		"model": model,
		"store": true,
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
		"document_id":   documentID,
		"document_name": documentName,
	}
	switch phase {
	case "warmup":
		metadataMap["spawn_mode"] = "document_warmup"
		metadataMap["document_task"] = task
	default:
		metadataMap["spawn_mode"] = "document_query"
		if strings.TrimSpace(bootstrapChildThreadID) != "" {
			metadataMap["bootstrap_child_thread_id"] = bootstrapChildThreadID
		}
	}

	childMetadataJSON, err := json.Marshal(metadataMap)
	if err != nil {
		return "", agentcmd.Command{}, fmt.Errorf("marshal child metadata: %w", err)
	}

	metadata, err := rawJSONToAny(childMetadataJSON)
	if err != nil {
		return "", agentcmd.Command{}, fmt.Errorf("decode child metadata: %w", err)
	}
	startBody["metadata"] = metadata

	body, err := json.Marshal(startBody)
	if err != nil {
		return "", agentcmd.Command{}, fmt.Errorf("marshal child start body: %w", err)
	}

	if err := a.store.CreateThreadIfAbsent(a.ctx, threadstore.ThreadMeta{
		ID:                 threadID,
		RootThreadID:       parentMeta.RootThreadID,
		ParentThreadID:     parentMeta.ID,
		ParentCallID:       parentCallID,
		Depth:              parentMeta.Depth + 1,
		Status:             threadstore.ThreadStatusNew,
		Model:              model,
		MetadataJSON:       string(childMetadataJSON),
		ActiveSpawnGroupID: spawnGroupID,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}); err != nil {
		return "", agentcmd.Command{}, err
	}

	causationID := parentMeta.LastResponseID
	if strings.TrimSpace(previousResponseID) != "" {
		causationID = previousResponseID
	}

	return threadID, agentcmd.Command{
		CmdID:        cmdID,
		Kind:         agentcmd.KindThreadStart,
		ThreadID:     threadID,
		RootThreadID: parentMeta.RootThreadID,
		CausationID:  causationID,
		Body:         body,
	}, nil
}

func stableDocumentSpawnGroupID(parentThreadID, parentCallID string) string {
	return stableDocumentDerivedID("sg_doc", parentThreadID, parentCallID)
}

func stableDocumentChildThreadID(parentThreadID, parentCallID, documentID, phase string) string {
	return stableDocumentDerivedID("thread_doc", parentThreadID, parentCallID, documentID, phase)
}

func stableDocumentChildCmdID(parentThreadID, parentCallID, documentID, phase string) string {
	return stableDocumentDerivedID("cmd_doc", parentThreadID, parentCallID, documentID, phase, "start")
}

func stableDocumentPreparedInputID(parentThreadID, parentCallID, documentID, phase string) string {
	return stableDocumentDerivedID("prep_doc", parentThreadID, parentCallID, documentID, phase)
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

func uniqueStringsPreserveOrder(values []string) []string {
	seen := make(map[string]bool, len(values))
	deduped := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		deduped = append(deduped, value)
	}
	return deduped
}

func decodeThreadMetadataJSON(raw string) (shared.Metadata, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return shared.Metadata{}, nil
	}
	return decodeMetadataParam(json.RawMessage(raw))
}

func (a *threadActor) startSpawnGroup(parentMeta threadstore.ThreadMeta, parentCallID string, request spawnRequest) (string, error) {
	spawnGroupID, err := idgen.New("sg")
	if err != nil {
		return "", err
	}

	childThreadIDs := make([]string, 0, len(request.Children))
	childCommands := make([]agentcmd.Command, 0, len(request.Children))
	childToolsJSON, err := filterSubagentTools(parentMeta.ToolsJSON)
	if err != nil {
		return "", err
	}
	childToolChoiceJSON, err := filterSubagentToolChoice(parentMeta.ToolChoiceJSON)
	if err != nil {
		return "", err
	}
	spawnMode := normalizeSpawnMode(request.SpawnMode)
	branchPreviousResponseID, err := resolveBranchPreviousResponseID(request, parentMeta)
	if err != nil {
		return "", err
	}

	for index, child := range request.Children {
		threadID := child.ThreadID
		if threadID == "" {
			threadID, err = idgen.New("thread")
			if err != nil {
				return "", err
			}
		}

		childInput, err := normalizeChildInput(child)
		if err != nil {
			return "", err
		}

		cmdID, err := idgen.New("cmd")
		if err != nil {
			return "", err
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
			normalizedInclude, err := agentcmd.NormalizeInclude(nil)
			if err != nil {
				return "", fmt.Errorf("normalize child include: %w", err)
			}
			childIncludeJSON = string(normalizedInclude)
		}
		childMetadataJSON, err = mergeMetadataJSON(childMetadataJSON, map[string]string{
			"spawn_mode":                spawnMode,
			"branch_parent_thread_id":   parentMeta.ID,
			"branch_parent_response_id": branchPreviousResponseID,
			"branch_index":              strconv.Itoa(index + 1),
		}, spawnMode == spawnModeWarmBranch)
		if err != nil {
			return "", fmt.Errorf("merge child metadata: %w", err)
		}
		if strings.TrimSpace(childMetadataJSON) != "" {
			metadata, err := rawJSONToAny(json.RawMessage(childMetadataJSON))
			if err != nil {
				return "", fmt.Errorf("decode child metadata: %w", err)
			}
			startBody["metadata"] = metadata
		}
		if strings.TrimSpace(childIncludeJSON) != "" {
			include, err := rawJSONToAny(json.RawMessage(childIncludeJSON))
			if err != nil {
				return "", fmt.Errorf("decode child include: %w", err)
			}
			startBody["include"] = include
		}
		if spawnMode == spawnModeWarmBranch {
			startBody["previous_response_id"] = branchPreviousResponseID
		}
		if strings.TrimSpace(childToolsJSON) != "" {
			tools, err := rawJSONToAny(json.RawMessage(childToolsJSON))
			if err != nil {
				return "", fmt.Errorf("decode child tools: %w", err)
			}
			startBody["tools"] = tools
		}
		if strings.TrimSpace(childToolChoiceJSON) != "" {
			toolChoice, err := rawJSONToAny(json.RawMessage(childToolChoiceJSON))
			if err != nil {
				return "", fmt.Errorf("decode child tool_choice: %w", err)
			}
			startBody["tool_choice"] = toolChoice
		}
		if strings.TrimSpace(parentMeta.ReasoningJSON) != "" {
			reasoning, err := rawJSONToAny(json.RawMessage(parentMeta.ReasoningJSON))
			if err != nil {
				return "", fmt.Errorf("decode child reasoning: %w", err)
			}
			startBody["reasoning"] = reasoning
		}

		body, err := json.Marshal(startBody)
		if err != nil {
			return "", fmt.Errorf("marshal child start body: %w", err)
		}

		childCommands = append(childCommands, agentcmd.Command{
			CmdID:        cmdID,
			Kind:         agentcmd.KindThreadStart,
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
			return "", err
		}

		a.logger.Info("spawning child thread",
			appendThreadGraphAttrs([]any{
				"spawn_group_id", spawnGroupID,
				"child_thread_id", threadID,
				"child_kind", "subagent",
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
		Expected:       len(childCommands),
		Status:         threadstore.SpawnGroupStatusWaiting,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}, childThreadIDs); err != nil {
		return "", err
	}

	a.logger.Info("opened child barrier",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawnGroupID,
			"child_source", "spawn_subagents",
			"spawn_mode", spawnMode,
			"expected_children", len(childCommands),
		}, parentMeta)...,
	)

	for _, cmd := range childCommands {
		if err := a.publish(a.ctx, agentcmd.DispatchStartSubject, cmd); err != nil {
			return "", err
		}
	}

	return spawnGroupID, nil
}

func (a *threadActor) publishChildTerminal(meta threadstore.ThreadMeta) error {
	if a.publish == nil || meta.ParentThreadID == "" || meta.ActiveSpawnGroupID == "" {
		return nil
	}

	parentMeta, err := a.store.LoadThread(a.ctx, meta.ParentThreadID)
	if err != nil {
		return err
	}

	cmdID, err := idgen.New("cmd")
	if err != nil {
		return err
	}

	status := string(meta.Status)
	assistantText, err := a.loadLatestAssistantText(meta.ID)
	if err != nil {
		a.logger.Warn("failed to load child assistant summary",
			"spawn_group_id", meta.ActiveSpawnGroupID,
			"error", err,
		)
		assistantText = ""
	}

	body, err := json.Marshal(agentcmd.ChildResultBody{
		SpawnGroupID:    meta.ActiveSpawnGroupID,
		ChildThreadID:   meta.ID,
		ChildResponseID: meta.LastResponseID,
		Status:          status,
		AssistantText:   assistantText,
	})
	if err != nil {
		return fmt.Errorf("marshal child terminal body: %w", err)
	}

	kind := agentcmd.KindThreadChildCompleted
	if meta.Status != threadstore.ThreadStatusCompleted {
		kind = agentcmd.KindThreadChildFailed
	}

	subject := agentcmd.DispatchAdoptSubject
	if parentMeta.OwnerWorkerID != "" {
		subject = agentcmd.WorkerCommandSubject(parentMeta.OwnerWorkerID, kind)
	} else {
		switch kind {
		case agentcmd.KindThreadChildCompleted:
			subject = "agent.dispatch.thread.child_completed"
		default:
			subject = "agent.dispatch.thread.child_failed"
		}
	}

	return a.publish(a.ctx, subject, agentcmd.Command{
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

func filterSubagentTools(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	tools, err := agentcmd.DecodeTools(json.RawMessage(raw))
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

func filterSubagentToolChoice(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	choice, err := decodeToolChoiceParam(json.RawMessage(raw))
	if err != nil {
		return "", fmt.Errorf("decode tool_choice for child filtering: %w", err)
	}

	filtered, ok := filterSubagentToolChoiceParam(choice)
	if !ok {
		return "", nil
	}

	payload, err := json.Marshal(filtered)
	if err != nil {
		return "", fmt.Errorf("marshal filtered child tool_choice: %w", err)
	}

	return string(payload), nil
}

func shouldPublishChildTerminal(status threadstore.ThreadStatus, meta threadstore.ThreadMeta) bool {
	if meta.ParentThreadID == "" || meta.ActiveSpawnGroupID == "" {
		return false
	}

	return isTerminalThreadStatus(status)
}

func shouldReleaseTerminalChildResources(meta threadstore.ThreadMeta) bool {
	return meta.ParentThreadID != "" && isTerminalThreadStatus(meta.Status)
}

func isTerminalThreadStatus(status threadstore.ThreadStatus) bool {
	switch status {
	case threadstore.ThreadStatusCompleted, threadstore.ThreadStatusFailed, threadstore.ThreadStatusCancelled, threadstore.ThreadStatusIncomplete:
		return true
	default:
		return false
	}
}

func validateCommandPreconditions(cmd agentcmd.Command, meta threadstore.ThreadMeta) error {
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
