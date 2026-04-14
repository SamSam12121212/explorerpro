package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"explorer/internal/config"
	"explorer/internal/docstore"
	"explorer/internal/documenthandler"
	"explorer/internal/idgen"
	"explorer/internal/natsbootstrap"
	"explorer/internal/openaiws"
	"explorer/internal/platform"
	"explorer/internal/postgresstore"
	"explorer/internal/threadcmd"
	"explorer/internal/threaddocstore"
	"explorer/internal/threadevents"
	"explorer/internal/threadhistory"
	"explorer/internal/threadstore"

	"github.com/nats-io/nats.go"
)

const (
	commandAckWait    = 30 * time.Minute
	workerLeaseTTL    = 2 * time.Minute
	workerConsumerTTL = 10 * time.Minute
	socketExpiryTTL   = 55 * time.Minute
	socketRotateLead  = 5 * time.Minute
	socketPruneTTL    = 1 * time.Hour
	socketSweepBatch  = 256
	commandQueueSize  = 128
	recoverySweepTTL  = 15 * time.Second
)

type Service struct {
	cfg          config.Config
	logger       *slog.Logger
	runtime      *platform.Runtime
	dialer       openaiws.Dialer
	openAIConfig openaiws.Config
	workerID     string
	store        *postgresstore.Store
	history      threadHistoryStore
	threadDocs   *threaddocstore.Store
	docClient    *documenthandler.Client
	docStore     docActorDocStore
	sweepStore   serviceSweepStore
	publishFn    func(ctx context.Context, subject string, cmd threadcmd.Command) error

	actorsMu sync.Mutex
	actors   map[int64]*threadActor
}

type serviceSweepStore interface {
	ListThreadIDsByStatus(ctx context.Context, status threadstore.ThreadStatus) ([]int64, error)
	LoadThread(ctx context.Context, threadID int64) (threadstore.ThreadMeta, error)
	LoadOwner(ctx context.Context, threadID int64) (threadstore.OwnerRecord, error)
	DisconnectExpiredOpenAISocketSessions(ctx context.Context, disconnectedAt, expiresAt time.Time, limit int) (int64, error)
	PruneExpiredOpenAISocketSessions(ctx context.Context, now time.Time, limit int) (int64, error)
}

func New(cfg config.Config, logger *slog.Logger, runtime *platform.Runtime, dialer openaiws.Dialer) (*Service, error) {
	openAIConfig := openaiws.FromAppConfig(cfg.OpenAI)
	if err := openAIConfig.Validate(); err != nil {
		return nil, fmt.Errorf("validate openai websocket config: %w", err)
	}

	if dialer == nil {
		return nil, fmt.Errorf("openai websocket dialer is required")
	}

	store := postgresstore.New(runtime.Postgres().Pool())
	history := threadhistory.New(runtime.NATS().JetStream())
	threadDocs := threaddocstore.New(runtime.Postgres().Pool())
	docs := docstore.New(runtime.Postgres().Pool())
	docClient := documenthandler.NewClient(runtime.NATS().Conn())

	workerID, err := newWorkerID()
	if err != nil {
		return nil, err
	}

	return &Service{
		cfg:          cfg,
		logger:       logger,
		runtime:      runtime,
		dialer:       dialer,
		openAIConfig: openAIConfig,
		workerID:     workerID,
		store:        store,
		history:      history,
		threadDocs:   threadDocs,
		docClient:    docClient,
		docStore:     docs,
		sweepStore:   store,
		actors:       map[int64]*threadActor{},
	}, nil
}

func (s *Service) Run(ctx context.Context) error {
	if err := natsbootstrap.EnsureThreadCommandStream(s.runtime.NATS().JetStream()); err != nil {
		return err
	}
	if err := natsbootstrap.EnsureThreadEventsStream(s.runtime.NATS().JetStream()); err != nil {
		return err
	}
	if err := natsbootstrap.EnsureThreadHistoryStream(s.runtime.NATS().JetStream()); err != nil {
		return err
	}

	dispatchCh := make(chan *nats.Msg, 256)
	workerCh := make(chan *nats.Msg, 256)

	dispatchSub, err := s.runtime.NATS().JetStream().ChanQueueSubscribe(
		"thread.dispatch.*",
		threadcmd.DispatchQueue,
		dispatchCh,
		nats.BindStream(threadcmd.StreamName),
		nats.Durable(threadcmd.DurableDispatchName()),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.AckWait(commandAckWait),
		nats.DeliverAll(),
	)
	if err != nil {
		return fmt.Errorf("subscribe dispatch commands: %w", err)
	}

	workerSub, err := s.runtime.NATS().JetStream().ChanSubscribe(
		threadcmd.WorkerCommandWildcard(s.workerID),
		workerCh,
		nats.BindStream(threadcmd.StreamName),
		nats.Durable(threadcmd.DurableWorkerName(s.workerID)),
		nats.InactiveThreshold(workerConsumerTTL),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.AckWait(commandAckWait),
		nats.DeliverAll(),
	)
	if err != nil {
		_ = dispatchSub.Unsubscribe()
		return fmt.Errorf("subscribe worker commands: %w", err)
	}

	s.logger.Info("worker service starting",
		"service", s.cfg.ServiceName,
		"worker_id", s.workerID,
		"responses_ws_url", s.openAIConfig.ResponsesSocketURL,
	)

	go s.consumeChannel(ctx, "dispatch", dispatchCh)
	go s.consumeChannel(ctx, "worker", workerCh)
	go s.runRecoveryLoop(ctx)

	<-ctx.Done()

	s.closeActors()

	s.logger.Info("worker service stopping",
		"worker_id", s.workerID,
		"reason", ctx.Err(),
	)

	// Do not unsubscribe JetStream durables here.
	// The NATS Go client deletes durable consumers on Unsubscribe/Drain
	// if it created them, which would force a full DeliverAll replay on
	// the next worker startup. We let the connection close preserve them.
	_ = dispatchSub
	_ = workerSub
	return nil
}

func (s *Service) WorkerID() string {
	return s.workerID
}

func (s *Service) consumeChannel(ctx context.Context, source string, ch <-chan *nats.Msg) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := s.dispatchMessage(ctx, msg); err != nil {
				s.logger.Error("failed to dispatch worker command",
					"source", source,
					"subject", msg.Subject,
					"error", err,
				)
				_ = msg.Nak()
			}
		}
	}
}

func (s *Service) dispatchMessage(ctx context.Context, msg *nats.Msg) error {
	cmd, err := threadcmd.Decode(msg.Data)
	if err != nil {
		s.logger.Error("invalid command payload", "subject", msg.Subject, "error", err)
		return msg.Term()
	}

	if kindFromSubject := threadcmd.SubjectToKind(msg.Subject); kindFromSubject != "" && cmd.Kind == "" {
		cmd.Kind = kindFromSubject
	}

	if cmd.Kind == "" {
		s.logger.Error("command subject did not resolve to a kind", "subject", msg.Subject, "cmd_id", cmd.CmdID)
		return msg.Term()
	}

	s.logger.Info("command dispatched to actor",
		append(threadcmd.LogAttrs(cmd),
			"subject", msg.Subject,
		)...,
	)

	actor := s.getActor(ctx, cmd.ThreadID)
	if ok := actor.Enqueue(queuedCommand{
		msg: msg,
		cmd: cmd,
	}); !ok {
		return fmt.Errorf("actor queue closed for thread %d", cmd.ThreadID)
	}

	return nil
}

func (s *Service) getActor(ctx context.Context, threadID int64) *threadActor {
	s.actorsMu.Lock()
	defer s.actorsMu.Unlock()

	if actor, ok := s.actors[threadID]; ok {
		if !actor.IsClosed() {
			return actor
		}
		delete(s.actors, threadID)
	}

	actor := newThreadActor(ctx, threadActorConfig{
		ThreadID:       threadID,
		WorkerID:       s.workerID,
		Logger:         s.logger.With("thread_id", threadID),
		Store:          s.store,
		History:        s.history,
		ThreadDocs:     s.threadDocs,
		DocRuntime:     s.docClient,
		DocStore:       s.docStore,
		PreparedInputs: s.docClient,
		Blob:           s.runtime.Blob(),
		OpenAIConfig:   s.openAIConfig,
		Publish:        s.publishCommand,
		PublishEvent:   s.publishThreadEvent,
		SessionFactory: func() *openaiws.Session { return openaiws.NewSession(s.openAIConfig, s.dialer) },
	})

	s.actors[threadID] = actor
	return actor
}

func (s *Service) closeActors() {
	s.actorsMu.Lock()
	actors := make([]*threadActor, 0, len(s.actors))
	for _, actor := range s.actors {
		actors = append(actors, actor)
	}
	s.actorsMu.Unlock()

	for _, actor := range actors {
		if err := actor.Close(); err != nil {
			s.logger.Warn("failed to close thread actor", "thread_id", actor.threadID, "error", err)
		}
	}
}

func newWorkerID() (string, error) {
	workerID, err := idgen.New("worker")
	if err != nil {
		return "", fmt.Errorf("generate worker id: %w", err)
	}

	return workerID, nil
}

type queuedCommand struct {
	msg *nats.Msg
	cmd threadcmd.Command
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}

	return *value
}

func rawJSONToAny(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode raw json payload: %w", err)
	}

	return decoded, nil
}

func (s *Service) publishThreadEvent(ctx context.Context, threadID int64, socketGeneration uint64, key string, eventType string, raw json.RawMessage) error {
	env := threadevents.EventEnvelope{
		ThreadID:         threadID,
		EventType:        eventType,
		SocketGeneration: socketGeneration,
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Payload:          raw,
	}

	data, err := threadevents.Encode(env)
	if err != nil {
		return fmt.Errorf("encode thread event for %d: %w", threadID, err)
	}

	msg := &nats.Msg{
		Subject: threadevents.Subject(threadID),
		Header:  nats.Header{},
		Data:    data,
	}
	msg.Header.Set("Nats-Msg-Id", threadevents.MsgID(threadID, socketGeneration, key))

	if _, err := s.runtime.NATS().JetStream().PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publish thread event for %d (%s): %w", threadID, eventType, err)
	}

	return nil
}

func (s *Service) publishCommand(ctx context.Context, subject string, cmd threadcmd.Command) error {
	if s.publishFn != nil {
		return s.publishFn(ctx, subject, cmd)
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Header:  nats.Header{},
		Data:    payload,
	}
	msg.Header.Set("Nats-Msg-Id", cmd.CmdID)

	if _, err := s.runtime.NATS().JetStream().PublishMsg(msg); err != nil {
		return fmt.Errorf("publish command to %s: %w", subject, err)
	}

	return nil
}

func (s *Service) runRecoveryLoop(ctx context.Context) {
	s.recoverThreads(ctx)

	ticker := time.NewTicker(recoverySweepTTL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recoverThreads(ctx)
		}
	}
}

func (s *Service) recoverThreads(ctx context.Context) {
	for _, status := range recoverySweepStatuses() {
		threadIDs, err := s.sweepStore.ListThreadIDsByStatus(ctx, status)
		if err != nil {
			s.logger.Warn("failed to list threads for recovery sweep", "status", status, "error", err)
			continue
		}

		for _, threadID := range threadIDs {
			if err := s.recoverThread(ctx, threadID); err != nil {
				s.logger.Warn("failed to enqueue recovery command", "thread_id", threadID, "status", status, "error", err)
			}
		}
	}

	s.scheduleSocketRotations(ctx)
	s.sweepOpenAISocketSessions(ctx)
}

func (s *Service) sweepOpenAISocketSessions(ctx context.Context) {
	now := time.Now().UTC()
	disconnectUntilExhausted := func() (int64, error) {
		var total int64
		for {
			count, err := s.sweepStore.DisconnectExpiredOpenAISocketSessions(ctx, now, now.Add(socketPruneTTL), socketSweepBatch)
			if err != nil {
				return total, err
			}
			total += count
			if count < socketSweepBatch {
				return total, nil
			}
		}
	}
	pruneUntilExhausted := func() (int64, error) {
		var total int64
		for {
			count, err := s.sweepStore.PruneExpiredOpenAISocketSessions(ctx, now, socketSweepBatch)
			if err != nil {
				return total, err
			}
			total += count
			if count < socketSweepBatch {
				return total, nil
			}
		}
	}

	disconnected, err := disconnectUntilExhausted()
	if err != nil {
		s.logger.Warn("failed to disconnect stale openai socket sessions", "error", err)
	} else if disconnected > 0 {
		s.logger.Info("disconnected stale openai socket sessions", "count", disconnected)
	}

	pruned, err := pruneUntilExhausted()
	if err != nil {
		s.logger.Warn("failed to prune expired openai socket sessions", "error", err)
	} else if pruned > 0 {
		s.logger.Info("pruned expired openai socket sessions", "count", pruned)
	}
}

func recoverySweepStatuses() []threadstore.ThreadStatus {
	return []threadstore.ThreadStatus{
		threadstore.ThreadStatusWaitingChildren,
		threadstore.ThreadStatusRunning,
		threadstore.ThreadStatusReconciling,
	}
}

func (s *Service) recoverThread(ctx context.Context, threadID int64) error {
	meta, err := s.sweepStore.LoadThread(ctx, threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return nil
		}
		return err
	}

	kind := recoveryKindForThread(meta)

	owner, err := s.sweepStore.LoadOwner(ctx, threadID)
	ownerKnown := err == nil
	if err != nil && !errors.Is(err, threadstore.ErrThreadNotFound) {
		return err
	}

	now := time.Now().UTC()
	ownerLive := ownerKnown && strings.TrimSpace(owner.WorkerID) != "" && owner.LeaseUntil.After(now)

	subject := threadcmd.DispatchSubject(kind)
	if kind == threadcmd.KindThreadAdopt {
		subject = threadcmd.DispatchAdoptSubject
	}

	if ownerLive {
		if owner.WorkerID != s.workerID {
			return nil
		}
		if s.hasLiveActor(threadID) {
			return nil
		}
		subject = threadcmd.WorkerCommandSubject(s.workerID, kind)
	}

	cmd, err := buildRecoveryCommand(meta, owner, kind)
	if err != nil {
		return err
	}

	return s.publishCommand(ctx, subject, cmd)
}

func (s *Service) hasLiveActor(threadID int64) bool {
	s.actorsMu.Lock()
	defer s.actorsMu.Unlock()

	actor, ok := s.actors[threadID]
	return ok && !actor.IsClosed()
}

func recoveryKindForThread(meta threadstore.ThreadMeta) threadcmd.Kind {
	if meta.Status == threadstore.ThreadStatusRunning || meta.Status == threadstore.ThreadStatusReconciling || meta.ActiveResponseID != "" {
		return threadcmd.KindThreadReconcile
	}

	return threadcmd.KindThreadAdopt
}

func buildRecoveryCommand(meta threadstore.ThreadMeta, owner threadstore.OwnerRecord, kind threadcmd.Kind) (threadcmd.Command, error) {
	bodyStruct := map[string]any{
		"previous_worker_id":  owner.WorkerID,
		"required_generation": owner.SocketGeneration,
	}

	body, err := json.Marshal(bodyStruct)
	if err != nil {
		return threadcmd.Command{}, fmt.Errorf("marshal recovery body: %w", err)
	}

	cmdID := fmt.Sprintf("recovery_%s_%s_%d_%s",
		strings.ReplaceAll(string(kind), ".", "_"),
		strconv.FormatInt(meta.ID, 10),
		maxUint64(meta.SocketGeneration, owner.SocketGeneration),
		strings.ReplaceAll(string(meta.Status), ".", "_"),
	)

	return threadcmd.Command{
		CmdID:                    cmdID,
		Kind:                     kind,
		ThreadID:                 meta.ID,
		RootThreadID:             defaultRootThreadID(meta),
		ExpectedSocketGeneration: owner.SocketGeneration,
		CreatedAt:                time.Now().UTC().Format(time.RFC3339),
		Body:                     body,
	}, nil
}

func (s *Service) scheduleSocketRotations(ctx context.Context) {
	statuses := []threadstore.ThreadStatus{
		threadstore.ThreadStatusReady,
		threadstore.ThreadStatusWaitingTool,
		threadstore.ThreadStatusWaitingChildren,
	}

	now := time.Now().UTC()
	for _, status := range statuses {
		threadIDs, err := s.sweepStore.ListThreadIDsByStatus(ctx, status)
		if err != nil {
			s.logger.Warn("failed to list threads for rotation sweep", "status", status, "error", err)
			continue
		}

		for _, threadID := range threadIDs {
			meta, err := s.sweepStore.LoadThread(ctx, threadID)
			if err != nil {
				if errors.Is(err, threadstore.ErrThreadNotFound) {
					continue
				}
				s.logger.Warn("failed to load thread during rotation sweep", "thread_id", threadID, "error", err)
				continue
			}
			if !shouldRotateSocket(meta, s.workerID, now) {
				continue
			}
			if !s.hasLiveActor(threadID) {
				continue
			}

			cmd, err := buildRotateCommand(meta, now)
			if err != nil {
				s.logger.Warn("failed to build rotate command", "thread_id", threadID, "error", err)
				continue
			}

			if err := s.publishCommand(ctx, threadcmd.WorkerCommandSubject(s.workerID, threadcmd.KindThreadRotateSocket), cmd); err != nil {
				s.logger.Warn("failed to publish rotate command", "thread_id", threadID, "error", err)
			}
		}
	}
}

func defaultRootThreadID(meta threadstore.ThreadMeta) int64 {
	if meta.RootThreadID > 0 {
		return meta.RootThreadID
	}
	return meta.ID
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func shouldRotateSocket(meta threadstore.ThreadMeta, workerID string, now time.Time) bool {
	if meta.OwnerWorkerID != workerID {
		return false
	}
	if meta.SocketGeneration == 0 || meta.SocketExpiresAt.IsZero() {
		return false
	}
	if !statusSupportsIdleSocket(meta.Status) {
		return false
	}
	return !meta.SocketExpiresAt.After(now.Add(socketRotateLead))
}

func buildRotateCommand(meta threadstore.ThreadMeta, now time.Time) (threadcmd.Command, error) {
	body, err := json.Marshal(threadcmd.RotateSocketBody{
		Reason:      "pre_expiry_rotation",
		ScheduledAt: now.Format(time.RFC3339),
	})
	if err != nil {
		return threadcmd.Command{}, fmt.Errorf("marshal rotate body: %w", err)
	}

	return threadcmd.Command{
		CmdID:                    fmt.Sprintf("rotate_%d_%d", meta.ID, meta.SocketGeneration),
		Kind:                     threadcmd.KindThreadRotateSocket,
		ThreadID:                 meta.ID,
		RootThreadID:             defaultRootThreadID(meta),
		ExpectedStatus:           string(meta.Status),
		ExpectedSocketGeneration: meta.SocketGeneration,
		ExpectedLastResponseID:   meta.LastResponseID,
		CreatedAt:                now.Format(time.RFC3339),
		Body:                     body,
	}, nil
}
