package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/client"
)

// `pgmctl promote` ends a join window whose publisher is lost
// (pg-manager B-025). A join window is the state where the cluster's
// failover record lets only its publisher promote, because every other
// peer was still cloning when it was published. If the publisher dies
// for good, no survivor promotes, by design, until an operator decides
// to abandon the publisher's unreplicated writes. This command records
// that decision through POST /v1/promotion-authorization.

// joinWindowStatus mirrors pgmanager.JoinWindowStatus.
type joinWindowStatus struct {
	Publisher           string           `json:"Publisher"`
	RecordRevision      uint64           `json:"RecordRevision"`
	RecordUpdatedAt     time.Time        `json:"RecordUpdatedAt"`
	PublisherHoldsLease bool             `json:"PublisherHoldsLease"`
	PublisherSilentFor  time.Duration    `json:"PublisherSilentFor"`
	Peers               []joinWindowPeer `json:"Peers"`
}

// joinWindowPeer mirrors pgmanager.JoinWindowPeer.
type joinWindowPeer struct {
	NodeID           string        `json:"NodeID"`
	DataState        string        `json:"DataState"`
	SystemIdentifier string        `json:"SystemIdentifier"`
	DataStateAge     time.Duration `json:"DataStateAge"`
	LSN              uint64        `json:"LSN"`
	LSNAge           time.Duration `json:"LSNAge"`
	HasLSN           bool          `json:"HasLSN"`
	Fenced           bool          `json:"Fenced"`
}

// promotionAuthorization mirrors pgmanager.PromotionAuthorization.
type promotionAuthorization struct {
	Kind                   string    `json:"kind"`
	Target                 string    `json:"target"`
	AbandonedPublisher     string    `json:"abandoned_publisher"`
	FailoverQuorumRevision uint64    `json:"failover_quorum_revision"`
	SystemIdentifier       string    `json:"system_identifier"`
	PublisherLastLSN       uint64    `json:"publisher_last_lsn"`
	PublisherLastLSNAt     time.Time `json:"publisher_last_lsn_at"`
	TargetLSNAtRequest     uint64    `json:"target_lsn_at_request"`
	RequestedAt            time.Time `json:"requested_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	Actor                  string    `json:"actor"`
	Reason                 string    `json:"reason"`
	RequestID              string    `json:"request_id"`
}

const promoteLong = `End a join window whose publisher is lost, by promoting NODE.

A join window is the state where the cluster's failover record lets only
its publisher promote: every other peer was still cloning when it was
published, so only the publisher holds every write it acknowledged. If
the publisher is gone for good, no survivor ever promotes on its own.

This command abandons every write the publisher acknowledged that NODE
has not received. It:

  1. reads the join window from the cluster and prints what is lost;
  2. asks for the typed cluster name (or --force --cluster NAME);
  3. records the authorization (POST /v1/promotion-authorization) on the
     node it talks to; any live node works, no leader is needed;
  4. waits until NODE is primary, unless --no-wait.

pg-manager refuses unless the publisher has been silent for its policy's
abandon window (60s by default), NODE holds the publisher's database,
and no live peer is ahead of NODE. It fences the publisher, so it cannot
promote when it returns; its abandoned writes can be inspected first.
Unfence it to let it re-clone.

Outside a join window, use 'pgmctl switchover --target NODE' or
'pgmctl failover'.`

const promoteExample = `  pgmctl promote node-a --accept-data-loss --reason "node-c volume lost, INC-1234"
  pgmctl promote --cancel`

func newPromoteCmd(app *AppContext) *cobra.Command {
	var (
		acceptDataLoss, noWait, cancelPending, replace bool
		reason                                         string
		ttl                                            time.Duration
	)
	c := &cobra.Command{
		Use:     "promote NODE --accept-data-loss --reason TEXT",
		Short:   "Promote NODE out of a join window whose publisher is lost (abandons writes)",
		Long:    promoteLong,
		Example: promoteExample,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cancelPending {
				if len(args) != 0 {
					return WithExitCode(ExitUsage, errors.New("--cancel takes no NODE"))
				}
				if err := app.Setup(); err != nil {
					return err
				}
				return runPromoteCancel(cmd, app)
			}
			if len(args) != 1 {
				return WithExitCode(ExitUsage, errors.New("NODE is required: pgmctl promote NODE --accept-data-loss --reason TEXT"))
			}
			if !acceptDataLoss {
				return WithExitCode(ExitUsage, fmt.Errorf(
					"--accept-data-loss is required: promoting %s abandons every write the join-window publisher acknowledged that %s has not received", args[0], args[0]))
			}
			if strings.TrimSpace(reason) == "" {
				return WithExitCode(ExitUsage, errors.New("--reason is required; it is recorded with the authorization and every event"))
			}
			if err := app.Setup(); err != nil {
				return err
			}
			return runPromote(cmd, app, promoteRequest{
				target: args[0], reason: reason, ttl: ttl, replace: replace, wait: !noWait,
			})
		},
	}
	c.Flags().BoolVar(&acceptDataLoss, "accept-data-loss", false, "Required: accept losing the publisher's unreplicated writes")
	c.Flags().StringVar(&reason, "reason", "", "Required: why the publisher is abandoned (recorded)")
	c.Flags().DurationVar(&ttl, "ttl", 0, "How long the authorization may wait for NODE to promote (default: the cluster policy, 2m)")
	c.Flags().BoolVar(&noWait, "no-wait", false, "Return once the authorization is recorded")
	c.Flags().BoolVar(&replace, "replace", false, "Replace a pending authorization")
	c.Flags().BoolVar(&cancelPending, "cancel", false, "Cancel the pending authorization (the publisher stays fenced)")
	return c
}

type promoteRequest struct {
	target, reason string
	ttl            time.Duration
	replace, wait  bool
}

func runPromote(cmd *cobra.Command, app *AppContext, req promoteRequest) error {
	out := cmd.OutOrStdout()
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	st, err := fetchEngineStatus(ctx, app)
	cancel()
	if err != nil {
		return err
	}
	if st.JoinWindow == nil {
		return WithExitCode(ExitSubjectNA, errors.New(
			"the cluster is not in a join window, so there is nothing to override; use 'pgmctl switchover --target NODE' or 'pgmctl failover'"))
	}
	jw := st.JoinWindow
	if err = printPromotePreflight(out, jw, req.target); err != nil {
		return err
	}
	if err = clusterAffectingPrompt(app, "promote", req.target); err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()
	body := map[string]any{
		"target":           req.target,
		"reason":           req.reason,
		"accept_data_loss": true,
		"replace":          req.replace,
		"ttl_seconds":      int(req.ttl / time.Second),
		"request_id":       client.NewRequestID(),
	}
	env, err := app.Client.PostJSON(ctx, "/v1/promotion-authorization", body)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "no_join_window" {
			return WithExitCode(ExitSubjectNA, err)
		}
		return err
	}
	printRequestID(cmd, env)
	var auth promotionAuthorization
	if err := json.Unmarshal(env.EngineResult, &auth); err != nil {
		return fmt.Errorf("decode authorization: %w", err)
	}
	_, _ = fmt.Fprintf(out, "authorized: %s may promote until %s; %s is fenced\n",
		auth.Target, auth.ExpiresAt.UTC().Format(time.RFC3339), auth.AbandonedPublisher)
	if !req.wait {
		return nil
	}
	return waitForPromotion(cmd, app, auth)
}

// fetchEngineStatus reads /v1/status and returns the engine block.
func fetchEngineStatus(ctx context.Context, app *AppContext) (*pgmanagerStatus, error) {
	env, err := app.Client.GetJSON(ctx, "/v1/status")
	if err != nil {
		return nil, err
	}
	st, _, err := decodeStatusEngine(env.EngineResult)
	return st, err
}

// printPromotePreflight shows the join window, each peer's evidence,
// and the known and unknown parts of the loss. It refuses a target
// that is not listed, before anything is written.
func printPromotePreflight(w io.Writer, jw *joinWindowStatus, target string) error {
	var pub, tgt *joinWindowPeer
	for i := range jw.Peers {
		switch jw.Peers[i].NodeID {
		case jw.Publisher:
			pub = &jw.Peers[i]
		case target:
			tgt = &jw.Peers[i]
		}
	}
	if target == jw.Publisher {
		return WithExitCode(ExitUsage, fmt.Errorf("%s is the join-window publisher; it needs no authorization", target))
	}
	if tgt == nil {
		return WithExitCode(ExitUsage, fmt.Errorf("%s is not a member of the cluster", target))
	}
	_, _ = fmt.Fprintf(w, "Join window open: failover record rev %d published by %s (asynchronous; no peer named)\n",
		jw.RecordRevision, jw.Publisher)
	switch {
	case jw.PublisherHoldsLease:
		_, _ = fmt.Fprintf(w, "%s holds the leader lease: it is alive, and pg-manager will refuse\n", jw.Publisher)
	case pub != nil && pub.HasLSN:
		_, _ = fmt.Fprintf(w, "%s silent for %s (lease absent, last LSN %s)\n",
			jw.Publisher, jw.PublisherSilentFor.Round(time.Second), pgLSN(pub.LSN))
	default:
		_, _ = fmt.Fprintf(w, "%s silent for %s (lease absent)\n", jw.Publisher, jw.PublisherSilentFor.Round(time.Second))
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "  %-10s %-9s %-21s %-12s %s\n", "NODE", "DATA", "IDENTITY", "LSN", "FENCED")
	for _, p := range jw.Peers {
		lsn, data, mark := "-", p.DataState, ""
		if p.HasLSN {
			lsn = pgLSN(p.LSN)
		}
		if data == "" {
			data = "-"
		}
		switch p.NodeID {
		case target:
			mark = "  <- target"
		case jw.Publisher:
			mark = "  <- publisher"
		}
		row := fmt.Sprintf("  %-10s %-9s %-21s %-12s %-6s%s", p.NodeID, data, orDash(p.SystemIdentifier), lsn, yesNo(p.Fenced), mark)
		_, _ = fmt.Fprintln(w, strings.TrimRight(row, " "))
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "Promoting %s abandons:\n", target)
	if pub != nil && pub.HasLSN && tgt.HasLSN {
		var known uint64
		if pub.LSN > tgt.LSN {
			known = pub.LSN - tgt.LSN
		}
		_, _ = fmt.Fprintf(w, "  - %d bytes of WAL %s published as flushed that %s has not received\n", known, jw.Publisher, target)
	}
	_, _ = fmt.Fprintf(w, "  - every write %s acknowledged after its last published position (unknown; asynchronous replication)\n", jw.Publisher)
	_, _ = fmt.Fprintf(w, "%s will be fenced. Unfence it once its writes are not needed, so it can be re-cloned.\n\n", jw.Publisher)
	return nil
}

// waitForPromotion follows /v1/status until the target is primary and
// the authorization is consumed (exit 0), the authorization ends without
// a promotion (exit 2), or its deadline passes (exit 124).
func waitForPromotion(cmd *cobra.Command, app *AppContext, auth promotionAuthorization) error {
	out := cmd.OutOrStdout()
	// The TTL, not ExpiresAt: the server's clock may differ from ours.
	deadline := time.Now().Add(auth.ExpiresAt.Sub(auth.RequestedAt) + 30*time.Second)
	_, _ = fmt.Fprintf(out, "waiting for %s to promote...\n", auth.Target)
	for {
		ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
		st, err := fetchEngineStatus(ctx, app)
		cancel()
		if err == nil {
			pending := st.PromotionAuthorization != nil && st.PromotionAuthorization.RequestID == auth.RequestID
			primary := st.PrimaryNodeID == auth.Target || st.LeaderNodeID == auth.Target
			switch {
			case !pending && st.JoinWindow == nil && primary:
				_, _ = fmt.Fprintf(out, "%s is primary; the join window is closed\n", auth.Target)
				return nil
			case !pending && st.JoinWindow != nil:
				return WithExitCode(ExitUnhealthy, fmt.Errorf(
					"the authorization ended without a promotion (rejected or expired); see 'pgmctl get history' for the promotion_authorization event and its reason (request_id=%s)", auth.RequestID))
			}
		}
		if time.Now().After(deadline) {
			return WithExitCode(ExitTimeout, fmt.Errorf("%s did not become primary before the authorization deadline", auth.Target))
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func runPromoteCancel(cmd *cobra.Command, app *AppContext) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout(app))
	defer cancel()
	env, err := app.Client.DeleteJSON(ctx, "/v1/promotion-authorization")
	if err != nil {
		return err
	}
	printRequestID(cmd, env)
	if len(env.EngineResult) == 0 || string(env.EngineResult) == "null" {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no promotion authorization was pending")
		return nil
	}
	var auth promotionAuthorization
	if err := json.Unmarshal(env.EngineResult, &auth); err != nil {
		return fmt.Errorf("decode authorization: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cancelled the authorization for %s (request_id=%s); %s stays fenced\n",
		auth.Target, auth.RequestID, auth.AbandonedPublisher)
	return nil
}

// pgLSN renders a WAL position the way PostgreSQL does (X/Y).
func pgLSN(lsn uint64) string {
	return fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
