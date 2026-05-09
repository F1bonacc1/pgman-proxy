// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build integration

// US4 / T051 — Token rotation without restart (FR-031). The compose
// harness uses control.auth.token_env; rotating means changing the
// env var inside the container and probing without a process restart.
// Since `docker compose exec` runs in a NEW shell each time, mutating
// the parent process's env via exec doesn't help — instead we use
// the file-based source path:
//
// 1. Mount /tmp into the container, write a token file there.
// 2. Hot-rewrite the token file.
// 3. Exec curl with the OLD token — must fail.
// 4. Exec curl with the NEW token — must succeed.
//
// To stay self-contained, this test rotates the in-process env var
// via `pkill -SIGUSR1` once we wire SIGUSR1 to re-read the env (out of
// scope for v1). Until then this test is documented but skipped.

package integration

import "testing"

func TestLCM_TokenRotation_WithoutRestart(t *testing.T) {
	t.Skip("token-file rotation harness requires file-based token source; current compose template uses env-based source. " +
		"Tracked alongside FR-031 follow-up that adds a token_file volume to docker-compose.test.yml.")
}
