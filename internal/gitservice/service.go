package gitservice

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"explorer/internal/gitcmd"
	"explorer/internal/natsbootstrap"
	"explorer/internal/repostore"

	"github.com/nats-io/nats.go"
)

type Service struct {
	logger      *slog.Logger
	js          nats.JetStreamContext
	repos       *repostore.Store
	dataDir     string
	githubToken string
}

func New(logger *slog.Logger, js nats.JetStreamContext, repos *repostore.Store, dataDir, githubToken string) *Service {
	return &Service{
		logger:      logger,
		js:          js,
		repos:       repos,
		dataDir:     dataDir,
		githubToken: strings.TrimSpace(githubToken),
	}
}

func (s *Service) Run(ctx context.Context) error {
	if err := natsbootstrap.EnsureGitCommandStream(s.js); err != nil {
		return fmt.Errorf("bootstrap git command stream: %w", err)
	}

	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("create git data dir %s: %w", s.dataDir, err)
	}

	ch := make(chan *nats.Msg, 64)
	sub, err := s.js.ChanQueueSubscribe(
		gitcmd.CloneSubject,
		gitcmd.CloneQueue,
		ch,
		nats.BindStream(gitcmd.StreamName),
		nats.ManualAck(),
		nats.AckWait(5*time.Minute),
		nats.MaxDeliver(3),
	)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", gitcmd.CloneSubject, err)
	}

	s.logger.Info("git service started", "data_dir", s.dataDir, "subject", gitcmd.CloneSubject)

	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			s.logger.Info("git service stopping", "reason", ctx.Err())
			return nil
		case msg := <-ch:
			s.handleClone(ctx, msg)
		}
	}
}

func (s *Service) handleClone(ctx context.Context, msg *nats.Msg) {
	cmd, err := gitcmd.DecodeClone(msg.Data)
	if err != nil {
		s.logger.Error("invalid clone command", "error", err)
		_ = msg.Term()
		return
	}

	s.logger.Info("processing clone",
		"repo_id", cmd.RepoID,
		"url", cmd.URL,
		"ref", cmd.Ref,
		"name", cmd.Name,
	)

	clonePath := filepath.Join(s.dataDir, cmd.RepoID, cmd.Name)

	if err := s.repos.UpdateStatus(ctx, cmd.RepoID, "cloning", clonePath, "", ""); err != nil {
		s.logger.Error("failed to update repo status to cloning", "repo_id", cmd.RepoID, "error", err)
		_ = msg.Nak()
		return
	}

	commitSHA, err := s.cloneRepo(ctx, cmd.URL, cmd.Ref, clonePath)
	if err != nil {
		errMsg := err.Error()
		s.logger.Error("clone failed", "repo_id", cmd.RepoID, "error", errMsg)
		if updateErr := s.repos.UpdateStatus(ctx, cmd.RepoID, "failed", clonePath, "", errMsg); updateErr != nil {
			s.logger.Error("failed to update repo status to failed", "repo_id", cmd.RepoID, "error", updateErr)
		}
		_ = msg.Ack()
		return
	}

	if err := s.repos.UpdateStatus(ctx, cmd.RepoID, "ready", clonePath, commitSHA, ""); err != nil {
		s.logger.Error("failed to update repo status to ready", "repo_id", cmd.RepoID, "error", err)
		_ = msg.Nak()
		return
	}

	s.logger.Info("clone complete",
		"repo_id", cmd.RepoID,
		"clone_path", clonePath,
		"commit_sha", commitSHA,
	)

	_ = msg.Ack()
}

func (s *Service) authenticatedURL(rawURL string) string {
	if s.githubToken == "" {
		return rawURL
	}
	if strings.HasPrefix(rawURL, "https://github.com/") {
		return strings.Replace(rawURL, "https://github.com/", "https://"+s.githubToken+"@github.com/", 1)
	}
	return rawURL
}

func (s *Service) cloneRepo(ctx context.Context, repoURL, ref, dest string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create parent dir: %w", err)
	}

	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("remove existing clone: %w", err)
		}
	}

	cloneCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	authURL := s.authenticatedURL(repoURL)
	args := []string{"clone", "--depth", "1", "--branch", ref, "--single-branch", authURL, dest}
	cloneCmd := exec.CommandContext(cloneCtx, "git", args...)

	var stderr bytes.Buffer
	cloneCmd.Stderr = &stderr

	if err := cloneCmd.Run(); err != nil {
		return "", fmt.Errorf("git clone: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	shaCtx, shaCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shaCancel()

	shaCmd := exec.CommandContext(shaCtx, "git", "rev-parse", "HEAD")
	shaCmd.Dir = dest

	out, err := shaCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}
