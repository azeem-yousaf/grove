package ui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
)

// onConflictsChanged runs after every prediction with the previous and new
// predictions, both indexing m.snap.States. feature/predict-notify fills it in.
func (m *Model) onConflictsChanged(prev, next []git.Prediction) tea.Cmd {
	return nil
}
