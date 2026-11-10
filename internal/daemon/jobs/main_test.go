package jobs_test

import (
	"os"
	"testing"

	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }
