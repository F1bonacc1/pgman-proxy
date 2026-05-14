package confirm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrRefused is returned when a user explicitly rejects a prompt.
var ErrRefused = errors.New("operation refused by operator")

// ErrNotTTY is returned when stdin/stdout are not a TTY and the
// caller did not pass an override flag.
var ErrNotTTY = errors.New("stdin/stdout is not a TTY and no override flag was supplied")

// Prompt is the single-resource [y/N] confirmation. Returns nil iff
// the operator types 'y' or 'yes' (case-insensitive). FR-028.
//
// When yesFlag is true (operator passed --yes / -y), this returns nil
// immediately. When stdin is not a TTY and yesFlag is false, this
// returns ErrNotTTY without prompting.
func Prompt(in io.Reader, out io.Writer, op, target, cluster string, yesFlag bool) error {
	if yesFlag {
		return nil
	}
	if !IsTTY(in, out) {
		return fmt.Errorf("%w: pass --yes to confirm %s on %s in cluster %s", ErrNotTTY, op, target, cluster)
	}
	if target == "" {
		fmt.Fprintf(out, "About to %s in cluster %s. Continue? [y/N]: ", op, cluster)
	} else {
		fmt.Fprintf(out, "About to %s %s in cluster %s. Continue? [y/N]: ", op, target, cluster)
	}
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return ErrRefused
	}
}

// ConfirmClusterName is the cluster-affecting typed-cluster-name
// confirmation. Returns nil iff the operator types the expected
// cluster name verbatim. FR-029.
//
// When forceFlag is true AND clusterMatchedByFlag is true (operator
// passed --force --cluster <name> AND <name> matched the live cluster
// id), this returns nil immediately. forceFlag alone (without
// matching --cluster <name>) is REFUSED.
func ConfirmClusterName(in io.Reader, out io.Writer, op, target, expected string, forceFlag, clusterMatchedByFlag bool) error {
	if forceFlag {
		if !clusterMatchedByFlag {
			return fmt.Errorf("--force requires a matching --cluster %s; refusing to %s without typed cluster-name confirmation", expected, op)
		}
		return nil
	}
	if !IsTTY(in, out) {
		return fmt.Errorf("%w: pass --force --cluster %s to confirm %s on %s", ErrNotTTY, expected, op, target)
	}
	if target == "" {
		fmt.Fprintf(out, "About to %s cluster %s.\n", op, expected)
	} else {
		fmt.Fprintf(out, "About to %s cluster %s on %s.\n", op, expected, target)
	}
	fmt.Fprintf(out, "Type the cluster name to confirm: ")
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	if strings.TrimSpace(line) == expected {
		return nil
	}
	return ErrRefused
}
