package gitenv

import (
	"slices"
	"testing"
)

func TestWithoutRepository(t *testing.T) {
	want := []string{"HOME=/home/example", "GIT_CONFIG_GLOBAL=/tmp/gitconfig", "GIT_CONFIG_SYSTEM=/etc/gitconfig", "GIT_CONFIG_NOSYSTEM=1", "GIT_DIR_OTHER=kept"}
	env := append(slices.Clone(want), "GIT_ALTERNATE_OBJECT_DIRECTORIES=other", "GIT_CONFIG=other", "GIT_CONFIG_PARAMETERS=other", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_99=remote.origin.url", "GIT_CONFIG_VALUE_99=other", "GIT_OBJECT_DIRECTORY=other", "GIT_DIR=", "GIT_WORK_TREE=other",
		"GIT_IMPLICIT_WORK_TREE=1", "GIT_GRAFT_FILE=other", "GIT_INDEX_FILE=other", "GIT_NO_REPLACE_OBJECTS=1", "GIT_REPLACE_REF_BASE=other",
		"GIT_PREFIX=other", "GIT_INTERNAL_SUPER_PREFIX=other", "GIT_SHALLOW_FILE=other", "GIT_COMMON_DIR=other")
	if got := WithoutRepository(env); !slices.Equal(got, want) {
		t.Fatalf("filtered environment = %v, want %v", got, want)
	}
}
