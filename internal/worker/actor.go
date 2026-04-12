package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"explorer/internal/agentcmd"
	"explorer/internal/blobstore"
	"explorer/internal/docstore"
	"explorer/internal/idgen"
	"explorer/internal/openaiws"
	"explorer/internal/threadevents"
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
	ThreadDocs     threadDocumentStore
	DocExec        *documentExec
	Blob           *blobstore.LocalStore
	OpenAIConfig   openaiws.Config
	Publish        func(ctx context.Context, subject string, cmd agentcmd.Command) error
	PublishEvent   func(ctx context.Context, threadID string, socketGeneration uint64, key string, eventType string, raw json.RawMessage)
	SessionFactory func() *openaiws.Session
}

type actorStore interface {
	CreateThreadIfAbsent(ctx context.Context, meta threadstore.ThreadMeta) error
	LoadThread(ctx context.Context, threadID string) (threadstore.ThreadMeta, error)
	SaveThread(ctx context.Context, meta threadstore.ThreadMeta) error
	CommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error)
	MarkCommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error)
	ClaimOwnership(ctx context.Context, threadID, workerID string, leaseUntil time.Time) (threadstore.ClaimResult, error)
	RenewOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64, leaseUntil time.Time) (bool, error)
	RotateOwnership(ctx context.Context, threadID, workerID string, currentGeneration uint64, leaseUntil, socketExpiresAt time.Time) (uint64, bool, error)
	ReleaseOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64) error
	AppendItem(ctx context.Context, entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error)
	AppendEvent(ctx context.Context, entry threadstore.EventLogEntry) error
	SaveResponseRaw(ctx context.Context, threadID, responseID string, payload json.RawMessage) error
	LoadLatestClientResponseCreatePayload(ctx context.Context, threadID string) (json.RawMessage, error)
	ListItems(ctx context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.ItemRecord, error)
	CreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta, childThreadIDs []string) error
	LoadSpawnGroup(ctx context.Context, spawnGroupID string) (threadstore.SpawnGroupMeta, error)
	SaveSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) error
	ListSpawnResults(ctx context.Context, spawnGroupID string) ([]threadstore.SpawnChildResult, error)
	UpsertSpawnResult(ctx context.Context, spawnGroupID string, result threadstore.SpawnChildResult) (bool, []threadstore.SpawnChildResult, error)
}

type threadDocumentStore interface {
	ListDocuments(ctx context.Context, threadID string, limit int64) ([]docstore.Document, error)
	FilterAttached(ctx context.Context, threadID string, documentIDs []string) ([]string, error)
}

type threadActor struct {
	threadID     string
	workerID     string
	logger       *slog.Logger
	store        actorStore
	threadDocs   threadDocumentStore
	docExec      *documentExec
	blob         *blobstore.LocalStore
	cfg          openaiws.Config
	publish      func(ctx context.Context, subject string, cmd agentcmd.Command) error
	publishEvent func(ctx context.Context, threadID string, socketGeneration uint64, key string, eventType string, raw json.RawMessage)

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
		threadDocs:     cfg.ThreadDocs,
		docExec:        cfg.DocExec,
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
	a.closeDocumentSessions(meta.ID)

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
				"thread_id", queued.cmd.ThreadID,
			)
			return queued.msg.Ack()
		case errors.Is(err, errCommandPrecond):
			a.logger.Warn("dropping command that failed thread preconditions",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
				"thread_id", queued.cmd.ThreadID,
				"error", err,
			)
			return queued.msg.Ack()
		case shouldDropMissingThreadCommand(err):
			a.logger.Info("dropping command for missing thread",
				"cmd_id", queued.cmd.CmdID,
				"kind", queued.cmd.Kind,
				"thread_id", queued.cmd.ThreadID,
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
				"thread_id", queued.cmd.ThreadID,
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
			"thread_id", queued.cmd.ThreadID,
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
		"thread_id", queued.cmd.ThreadID,
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

	a.logger.Info("starting thread",
		"cmd_id", cmd.CmdID,
		"model", body.Model,
		"has_previous_response_id", strings.TrimSpace(body.PreviousResponseID) != "",
	)

	if strings.TrimSpace(body.PreviousResponseID) != "" && !boolOrDefault(body.Store, true) {
		return fmt.Errorf("thread.start previous_response_id requires store=true")
	}
	body.InitialInput, err = normalizeInputItems(body.InitialInput)
	if err != nil {
		return fmt.Errorf("normalize thread.start initial_input: %w", err)
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
	if err := a.appendInputItems(meta.ID, inputResponseID, body.InitialInput); err != nil {
		return err
	}

	effectiveInstructions, err := a.effectiveInstructions(meta.ID, body.Instructions)
	if err != nil {
		return err
	}

	payload, err := a.buildThreadResponseCreatePayload(meta, map[string]any{
		"model":                body.Model,
		"instructions":         effectiveInstructions,
		"input":                body.InitialInput,
		"metadata":             body.Metadata,
		"include":              body.Include,
		"tools":                body.Tools,
		"tool_choice":          body.ToolChoice,
		"reasoning":            body.Reasoning,
		"store":                boolOrDefault(body.Store, true),
		"previous_response_id": body.PreviousResponseID,
	})
	if err != nil {
		return err
	}

	return a.sendAndStream(meta, cmd.CmdID, payload)
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

	if err := a.ensureSession(); err != nil {
		return err
	}

	if err := a.appendInputItems(meta.ID, meta.LastResponseID, body.InputItems); err != nil {
		return err
	}

	return a.continueWithInputItems(meta, cmd.CmdID, body.InputItems)
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

	return a.continueWithInputItems(meta, cmd.CmdID, inputItems)
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

	assistantText, err := a.loadLatestAssistantText(body.ChildThreadID)
	if err != nil {
		return err
	}

	stored, results, err := a.store.UpsertSpawnResult(a.ctx, spawnGroupID, threadstore.SpawnChildResult{
		ChildThreadID:   body.ChildThreadID,
		Status:          status,
		ChildResponseID: body.ChildResponseID,
		AssistantText:   assistantText,
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

		if err := a.ensureSession(); err != nil {
			return err
		}

		if err := a.appendInputItems(meta.ID, meta.LastResponseID, inputItems); err != nil {
			return err
		}

		return a.continueWithInputItems(meta, cmd.CmdID, inputItems)
	}

	if err := a.store.SaveSpawnGroup(a.ctx, spawn); err != nil {
		return err
	}

	a.startIdleLoop(meta)
	return nil
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
			a.logger.Warn("failed to close rotated socket", "thread_id", meta.ID, "error", err)
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

	return a.store.AppendEvent(a.ctx, threadstore.EventLogEntry{
		ThreadID:         meta.ID,
		SocketGeneration: meta.SocketGeneration,
		EventType:        "client.socket.rotate",
		PayloadJSON:      string(payload),
		CreatedAt:        now,
	})
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
	a.closeDocumentSessions(meta.ID)

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
		"thread_id", meta.ID,
		"socket_generation", meta.SocketGeneration,
	)

	a.stopIdleLoop()
	a.stopLeaseLoop()
	a.resetSession()
	a.closeDocumentSessions(meta.ID)

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

	return a.continueWithInputItems(meta, cmdID, inputItems)
}

func (a *threadActor) reconcileFromCheckpoint(meta threadstore.ThreadMeta, cmdID string) error {
	rawEvent, err := a.store.LoadLatestClientResponseCreatePayload(a.ctx, meta.ID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
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

	return a.sendAndStream(meta, cmdID, payload)
}

func (a *threadActor) handleMissingRecoveryCheckpoint(meta threadstore.ThreadMeta) error {
	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession()
	a.closeDocumentSessions(meta.ID)

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
		"thread_id", meta.ID,
		"status", meta.Status,
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
			"previous_state", snapshot.State,
			"socket_generation", a.currentSocketGeneration(),
			"session_connect_count", snapshot.SocketGeneration,
		)
	}

	return a.reconnectSession()
}

func (a *threadActor) reconnectSession() error {
	a.logger.Info("connecting openai websocket session",
		"socket_generation", a.currentSocketGeneration(),
	)

	newSession, err := a.openFreshSession()
	if err != nil {
		a.logger.Error("failed to connect openai websocket session", "error", err)
		return err
	}

	a.logger.Info("openai websocket session connected",
		"socket_generation", a.currentSocketGeneration(),
		"session_connect_count", newSession.Snapshot().SocketGeneration,
	)

	oldSession := a.swapSession(newSession)
	if oldSession != nil {
		if err := oldSession.Close(); err != nil {
			a.logger.Warn("failed to close previous socket", "thread_id", a.threadID, "error", err)
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
				a.logger.Warn("failed to renew thread lease", "thread_id", meta.ID, "socket_generation", meta.SocketGeneration, "error", err)
				continue
			}
			if !renewed {
				a.logger.Warn("lost thread lease", "thread_id", meta.ID, "socket_generation", meta.SocketGeneration)
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
	a.closeDocumentSessions(a.threadID)

	a.cancel()
}

func (a *threadActor) sendAndStream(meta threadstore.ThreadMeta, eventID string, payload map[string]any) error {
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

	wirePayload, stats, err := a.lowerResponseCreatePayload(payload)
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
		"model", meta.Model,
		"reasoning_effort", responseCreateReasoningEffort(wirePayload),
		"socket_generation", meta.SocketGeneration,
		"input_items_count", stats.InputItemsCount,
		"lowered_image_inputs", stats.LoweredImageInputs,
		"lowered_blob_refs", stats.LoweredBlobRefs,
	)

	if err := a.sendResponseCreate(event); err != nil {
		return err
	}

	if err := a.store.AppendEvent(a.ctx, threadstore.EventLogEntry{
		ThreadID:         meta.ID,
		SocketGeneration: meta.SocketGeneration,
		EventType:        "client.response.create",
		PayloadJSON:      string(rawEvent),
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		return err
	}

	if a.publishEvent != nil {
		a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, "client-response-create", threadevents.EventTypeClientResponse, rawEvent)
	}

	return a.streamUntilTerminal(meta)
}

func (a *threadActor) lowerResponseCreatePayload(payload map[string]any) (map[string]any, payloadLoweringStats, error) {
	cloned := cloneAny(payload)
	root, ok := cloned.(map[string]any)
	if !ok {
		return nil, payloadLoweringStats{}, fmt.Errorf("response.create payload must be an object")
	}

	stats := payloadLoweringStats{}
	input, ok := root["input"]
	if !ok {
		return root, stats, nil
	}

	lowered, err := a.lowerInputValue(input, &stats)
	if err != nil {
		return nil, payloadLoweringStats{}, err
	}
	root["input"] = lowered
	return root, stats, nil
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
	items, ok := value.([]any)
	if !ok {
		return value, nil
	}
	stats.InputItemsCount = len(items)

	lowered := make([]any, len(items))
	for index, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			lowered[index] = item
			continue
		}

		loweredItem, err := a.lowerInputItem(itemMap, stats)
		if err != nil {
			return nil, err
		}
		lowered[index] = loweredItem
	}

	return lowered, nil
}

func (a *threadActor) lowerInputItem(item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	if item["type"] != "message" {
		return item, nil
	}

	content, ok := item["content"].([]any)
	if !ok {
		return item, nil
	}

	loweredContent := make([]any, len(content))
	for index, part := range content {
		partMap, ok := part.(map[string]any)
		if !ok {
			loweredContent[index] = part
			continue
		}

		loweredPart, err := a.lowerMessageContentItem(partMap, stats)
		if err != nil {
			return nil, err
		}
		loweredContent[index] = loweredPart
	}

	item["content"] = loweredContent
	return item, nil
}

func (a *threadActor) lowerMessageContentItem(item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	typeName := stringValue(item["type"])
	switch typeName {
	case "image_ref":
		stats.LoweredImageInputs++
		stats.LoweredBlobRefs++
		return a.buildInputImageFromBlobRef(item, stringValue(item["image_ref"]))
	case "input_image":
		imageURL := stringValue(item["image_url"])
		if strings.HasPrefix(imageURL, "blob://") {
			stats.LoweredImageInputs++
			stats.LoweredBlobRefs++
			return a.buildInputImageFromBlobRef(item, imageURL)
		}
	}

	return item, nil
}

func (a *threadActor) buildInputImageFromBlobRef(item map[string]any, ref string) (map[string]any, error) {
	if a.blob == nil {
		return nil, fmt.Errorf("blob store is not configured")
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("image input is missing image_ref")
	}

	data, err := a.blob.ReadRef(a.ctx, ref)
	if err != nil {
		return nil, err
	}

	contentType := strings.TrimSpace(stringValue(item["content_type"]))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return nil, fmt.Errorf("blob ref %s is not an image (detected %s)", ref, contentType)
	}

	detail := strings.TrimSpace(stringValue(item["detail"]))
	if detail == "" {
		detail = "auto"
	}

	dataURL := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return map[string]any{
		"type":      "input_image",
		"image_url": dataURL,
		"detail":    detail,
	}, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (a *threadActor) continueWithInputItems(meta threadstore.ThreadMeta, cmdID string, inputItems json.RawMessage) error {
	effectiveInstructions, err := a.effectiveInstructions(meta.ID, meta.Instructions)
	if err != nil {
		return err
	}

	payload, err := a.buildThreadResponseCreatePayload(meta, map[string]any{
		"model":                meta.Model,
		"instructions":         effectiveInstructions,
		"input":                inputItems,
		"previous_response_id": meta.LastResponseID,
		"store":                true,
	})
	if err != nil {
		return err
	}

	return a.sendAndStream(meta, cmdID, payload)
}

func (a *threadActor) effectiveInstructions(threadID, base string) (string, error) {
	if a.threadDocs == nil {
		return base, nil
	}

	documents, err := a.threadDocs.ListDocuments(a.ctx, threadID, 200)
	if err != nil {
		return "", fmt.Errorf("list thread documents for response.create instructions: %w", err)
	}
	if len(documents) == 0 {
		return base, nil
	}

	block := formatAvailableDocumentsBlock(documents)
	if block == "" {
		return base, nil
	}

	trimmedBase := strings.TrimRight(base, "\n")
	if strings.TrimSpace(trimmedBase) == "" {
		return block, nil
	}

	return trimmedBase + "\n\n" + block, nil
}

func formatAvailableDocumentsBlock(documents []docstore.Document) string {
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
		builder.WriteString(escapePromptAttribute(id))
		builder.WriteString(`" name="`)
		builder.WriteString(escapePromptAttribute(name))
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

func escapePromptAttribute(value string) string {
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

const toolNameQueryAttachedDocuments = "query_attached_documents"

func (a *threadActor) injectDocumentTools(threadID string, payload map[string]any) error {
	if a.threadDocs == nil {
		return nil
	}

	docs, err := a.threadDocs.ListDocuments(a.ctx, threadID, 1)
	if err != nil {
		return fmt.Errorf("check thread documents for tool injection: %w", err)
	}
	if len(docs) == 0 {
		return nil
	}

	existing, err := decodePayloadTools(payload["tools"])
	if err != nil {
		return fmt.Errorf("decode tools for document injection: %w", err)
	}
	for _, tool := range existing {
		if toolParamName(tool) == toolNameQueryAttachedDocuments {
			return nil
		}
	}

	payload["tools"] = append(existing, queryAttachedDocumentsToolDef())
	return nil
}

type docQueryRequest struct {
	DocumentIDs []string `json:"document_ids"`
	Task        string   `json:"task"`
}

func decodeDocQueryRequest(arguments string) (docQueryRequest, error) {
	var req docQueryRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return docQueryRequest{}, fmt.Errorf("decode %s arguments: %w", toolNameQueryAttachedDocuments, err)
	}
	if len(req.DocumentIDs) == 0 {
		return docQueryRequest{}, fmt.Errorf("%s requires at least one document_id", toolNameQueryAttachedDocuments)
	}
	if strings.TrimSpace(req.Task) == "" {
		return docQueryRequest{}, fmt.Errorf("%s requires a non-empty task", toolNameQueryAttachedDocuments)
	}
	return req, nil
}

func (a *threadActor) handlePendingDocumentQuery(meta threadstore.ThreadMeta, callID string, req docQueryRequest) error {
	output := a.executeDocumentQuery(meta, req)

	outputItem, err := json.Marshal(map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	})
	if err != nil {
		return fmt.Errorf("marshal document query output: %w", err)
	}

	cmdID, err := idgen.New("cmd")
	if err != nil {
		return err
	}

	body, err := json.Marshal(agentcmd.SubmitToolOutputBody{
		CallID:     callID,
		OutputItem: outputItem,
	})
	if err != nil {
		return fmt.Errorf("marshal document query submit body: %w", err)
	}

	subject := agentcmd.WorkerCommandSubject(a.workerID, agentcmd.KindThreadSubmitToolOutput)
	return a.publish(a.ctx, subject, agentcmd.Command{
		CmdID:        cmdID,
		Kind:         agentcmd.KindThreadSubmitToolOutput,
		ThreadID:     meta.ID,
		RootThreadID: meta.RootThreadID,
		CausationID:  meta.LastResponseID,
		Body:         body,
	})
}

func (a *threadActor) executeDocumentQuery(meta threadstore.ThreadMeta, req docQueryRequest) string {
	if a.threadDocs == nil {
		return `{"error":"document store not available"}`
	}

	attached, err := a.threadDocs.FilterAttached(a.ctx, meta.ID, req.DocumentIDs)
	if err != nil {
		a.logger.Warn("failed to validate attached documents for tool call",
			"thread_id", meta.ID,
			"error", err,
		)
		return `{"error":"failed to validate attached documents"}`
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
		result, _ := json.Marshal(map[string]any{
			"error":        "some requested documents are not attached to this thread",
			"missing_ids":  missing,
			"attached_ids": attached,
		})
		return string(result)
	}

	if a.docExec == nil {
		result, _ := json.Marshal(map[string]any{
			"status":       "not_yet_implemented",
			"document_ids": req.DocumentIDs,
			"task":         req.Task,
			"message":      "Document executor is not configured.",
		})
		return string(result)
	}

	a.logger.Info("executing document query",
		"document_ids", req.DocumentIDs,
		"task_length", len(req.Task),
		"model", meta.Model,
	)

	results := a.docExec.Execute(a.ctx, docExecRequest{
		ThreadID:    meta.ID,
		DocumentIDs: req.DocumentIDs,
		Task:        req.Task,
		Model:       meta.Model,
	})

	resultJSON, _ := json.Marshal(map[string]any{
		"results": results,
	})
	return string(resultJSON)
}

func (a *threadActor) streamUntilTerminal(meta threadstore.ThreadMeta) error {
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return openaiws.ErrNotConnected
	}

	a.logger.Info("streaming response events from openai",
		"socket_generation", meta.SocketGeneration,
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
		if err := a.store.AppendEvent(a.ctx, threadstore.EventLogEntry{
			ThreadID:         meta.ID,
			SocketGeneration: meta.SocketGeneration,
			EventType:        string(event.Type),
			ResponseID:       responseID,
			PayloadJSON:      string(event.Raw),
			CreatedAt:        time.Now().UTC(),
		}); err != nil {
			return err
		}

		if a.publishEvent != nil {
			a.publishEvent(a.ctx, meta.ID, meta.SocketGeneration, fmt.Sprintf("event-%d", eventCount), string(event.Type), event.Raw)
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
					} else if err == nil && call.Name == toolNameQueryAttachedDocuments {
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
				if err := a.handlePendingDocumentQuery(meta, pendingDocQueryCallID, *pendingDocQuery); err != nil {
					return err
				}
				meta.Status = threadstore.ThreadStatusWaitingTool
			} else if waitingTool || pendingDocQuery != nil {
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
				"from", prevStatus,
				"to", meta.Status,
				"event_type", event.Type,
				"response_id", responseID,
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
				"final_status", meta.Status,
				"last_response_id", meta.LastResponseID,
				"events_received", eventCount,
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
					"thread_id", meta.ID,
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
			"thread_id", a.threadID,
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
		a.logger.Warn("failed to marshal thread snapshot event", "thread_id", meta.ID, "error", err)
		return
	}

	a.publishEvent(
		a.ctx,
		meta.ID,
		meta.SocketGeneration,
		fmt.Sprintf("snapshot-%d", meta.UpdatedAt.UnixNano()),
		threadevents.EventTypeThreadSnapshot,
		payload,
	)
}

func (a *threadActor) publishThreadItem(item threadstore.ItemRecord) {
	if a.publishEvent == nil {
		return
	}

	payload, err := json.Marshal(presentThreadItem(item))
	if err != nil {
		a.logger.Warn("failed to marshal thread item event", "thread_id", a.threadID, "seq", item.Seq, "error", err)
		return
	}

	a.publishEvent(
		a.ctx,
		a.threadID,
		a.currentSocketGeneration(),
		fmt.Sprintf("item-%d", item.Seq),
		threadevents.EventTypeThreadItem,
		payload,
	)
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
	if item.StreamID != "" {
		response["stream_id"] = item.StreamID
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

func (a *threadActor) closeDocumentSessions(threadID string) {
	if a.docExec == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	if err := a.docExec.CloseThread(threadID); err != nil {
		a.logger.Warn("failed to close document sessions", "thread_id", threadID, "error", err)
	}
}

func (a *threadActor) releaseTerminalChildResources(meta *threadstore.ThreadMeta) error {
	a.stopLeaseLoop()
	a.stopIdleLoop()
	a.resetSession()
	a.closeDocumentSessions(meta.ID)

	if meta.OwnerWorkerID == a.workerID && meta.SocketGeneration > 0 {
		if err := a.store.ReleaseOwnership(a.ctx, meta.ID, a.workerID, meta.SocketGeneration); err != nil {
			return err
		}
	}

	meta.OwnerWorkerID = ""
	meta.SocketExpiresAt = time.Time{}
	a.setMeta(*meta)

	a.logger.Info("released terminal child socket",
		"socket_generation", meta.SocketGeneration,
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
	body, err := json.Marshal(map[string]any{
		"spawn_group_id":    meta.ActiveSpawnGroupID,
		"child_thread_id":   meta.ID,
		"child_response_id": meta.LastResponseID,
		"status":            status,
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
