package ui

import "testing"

func TestLooksLikeQuestion(t *testing.T) {
	asks := []string{
		"Should the API return JSON or XML?",
		"Done with the API.\n\n**Which one do you want?**",
		"I've drafted two options. Let me know which you prefer.",
		"Should I also update the docs.",
		"Please confirm the port before I continue.",
		"The schema is ready.\n\nI'm waiting for your decision on the column names.",
		"Could you run the migration for me.",
		"Would you like me to add tests as well.",
		"Two ways to do it:\n\n1. A\n2. B\n\nWhich would you prefer: A or B",
		"Do you want the old endpoint kept",
		"Shall I go ahead with the rename.",
	}
	finished := []string{
		"All done. Tests pass.",
		"Done: added the endpoint and its tests. Let me know if you need anything else.",
		"Finished the refactor. Let me know if you have any questions.",
		"Implemented it. Would you like any other changes, just say.",
		"Should I add tests? I did: all pass.\n\nEverything is committed.",
		"It should be fine now; I could not reproduce the bug after the fix.",
		"",
	}
	for _, r := range asks {
		if !looksLikeQuestion(r) {
			t.Errorf("should ask: %q", r)
		}
	}
	for _, r := range finished {
		if looksLikeQuestion(r) {
			t.Errorf("shouldn't ask: %q", r)
		}
	}
}
