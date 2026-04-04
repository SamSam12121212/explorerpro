package gitcmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName   = "GIT_CMD"
	CloneSubject = "git.clone"
	CloneQueue   = "git-workers"
)

type CloneCommand struct {
	CmdID  string `json:"cmd_id"`
	RepoID string `json:"repo_id"`
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Name   string `json:"name"`
}

func DecodeClone(data []byte) (CloneCommand, error) {
	var cmd CloneCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return CloneCommand{}, fmt.Errorf("decode clone command: %w", err)
	}
	if strings.TrimSpace(cmd.RepoID) == "" {
		return CloneCommand{}, fmt.Errorf("clone command missing repo_id")
	}
	if strings.TrimSpace(cmd.URL) == "" {
		return CloneCommand{}, fmt.Errorf("clone command missing url")
	}
	return cmd, nil
}
