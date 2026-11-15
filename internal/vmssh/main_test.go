package vmssh

import (
	"testing"

	"github.com/devvmhq/devvm/internal/leaktest"
)

// TestMain fails the tests if a goroutine of theirs outlives them.
func TestMain(m *testing.M) { leaktest.Main(m) }
