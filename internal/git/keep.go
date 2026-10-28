package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// PathKey is a short, file-name-safe key for a folder, the same for every
// spelling of it that SamePath accepts.
func PathKey(path string) string {
	p := filepath.Clean(path)
	if filepath.Separator == '\\' {
		p = strings.ToLower(p)
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:8])
}

// checkpointRef is where the checkpoint of the worktree at path is kept.
func checkpointRef(path string) string { return "refs/grove/checkpoints/" + PathKey(path) }

// KeepCheckpoint points a ref at an agent's checkpoint tree so git gc
// doesn't delete it while grove may still need it for undo, including after
// grove restarts. dir is any folder of the repository; refs/grove is shared
// by every worktree.
func KeepCheckpoint(ctx context.Context, dir, worktree, tree string) error {
	_, err := run(ctx, dir, "update-ref", checkpointRef(worktree), tree)
	return err
}

// DropCheckpoint deletes the ref KeepCheckpoint made for worktree, if any.
func DropCheckpoint(ctx context.Context, dir, worktree string) error {
	_, err := run(ctx, dir, "update-ref", "-d", checkpointRef(worktree))
	return err
}

// HasObject reports whether the repository still has the object oid.
func HasObject(ctx context.Context, dir, oid string) bool {
	_, err := run(ctx, dir, "cat-file", "-e", oid)
	return err == nil
}
