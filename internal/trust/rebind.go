package trust

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
)

// ErrTrustRebindMissing is returned when the source approval does not
// exist for `wezsesh trust --rebind` (§13.5.2). Sentinel — `errors.Is`
// against this value when surfacing the `TRUST_REBIND_MISSING` exit code.
var ErrTrustRebindMissing = errors.New("trust: source approval not found")

// ErrTrustRebindDiverged is returned when the new path's command bytes
// differ from the old path's. The caller (`wezsesh trust --rebind`)
// must surface this as a refusal — silent uplift of approval scope is
// the threat model that motivates this whole code path (§13.5.2).
var ErrTrustRebindDiverged = errors.New("trust: command bytes diverged at new path")

// Rebind transfers approval from oldPath → newPath WITHOUT re-prompting,
// when both paths resolve to byte-equal command bytes. cmdBytes MUST be
// the in-memory bytes the caller already verified are identical at both
// paths (the caller reads each sidecar exactly once — same read-once
// rule that governs hook exec).
//
// Sequence (§8.12, §13.5.2):
//   - oldHash := ComputeHash(oldPath, cmdBytes)
//   - if oldHash file does not exist (or is a symlink) → ErrTrustRebindMissing
//   - newHash := ComputeHash(newPath, cmdBytes)
//   - write newHash file (atomic via safefs.AtomicWriteFile)
//   - remove oldHash file
//
// On any failure between Approve(new) and Revoke(old), the old approval
// is left intact. The new file is also left in place: the resulting
// state is "approved at both paths", which is identical to the user
// running `wezsesh trust <new>` followed by leaving the old approval —
// no scope uplift, just a leftover entry that `Prune` will reap when
// oldPath stops existing on disk.
func (s *Store) Rebind(ctx context.Context, oldPath, newPath string, cmdBytes []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if oldPath == "" {
		return errors.New("trust: Rebind: empty oldPath")
	}
	if newPath == "" {
		return errors.New("trust: Rebind: empty newPath")
	}
	if !s.IsApproved(ctx, oldPath, cmdBytes) {
		return ErrTrustRebindMissing
	}
	// Self-rebind is a no-op. Without this short-circuit Approve(new) and
	// Revoke(old) target the same hash file, so the (idempotent) Approve
	// is followed by a Revoke that deletes the just-written file, silently
	// destroying the approval and returning nil. The caller's intent on
	// oldPath == newPath is "leave the approval in place", so honour it.
	if oldPath == newPath {
		return nil
	}
	if err := s.Approve(ctx, newPath, cmdBytes); err != nil {
		return fmt.Errorf("trust: rebind approve new: %w", err)
	}
	if err := s.Revoke(ctx, oldPath, cmdBytes); err != nil {
		return fmt.Errorf("trust: rebind revoke old: %w", err)
	}
	return nil
}

// VerifyRebindEligible reads the sidecar at sidecarPath (via
// safefs.SafeOpenForRead), returns the in-memory command bytes IF AND
// ONLY IF they are byte-equal to expectedCmd. The sidecar is read
// exactly once; the same in-memory bytes returned here must be reused
// by the caller for any subsequent trust check or hook exec.
//
// CLI surface lives in `cmd/wezsesh/trust.go`; this helper exists so
// the byte-equality predicate has a single home (constant-time compare,
// length check first to avoid the O(n) compare on a length mismatch).
//
// Returns:
//   - nil bytes, nil err: file does not exist or is a symlink (caller
//     decides how to surface; the `--rebind` CLI surfaces "missing").
//   - non-nil bytes, nil err: bytes equal expectedCmd; rebind eligible.
//   - any bytes, ErrTrustRebindDiverged: bytes differ from expectedCmd.
func VerifyRebindEligible(sidecarPath string, expectedCmd []byte, readSidecar func(string) ([]byte, error)) ([]byte, error) {
	got, err := readSidecar(sidecarPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// Length check first short-circuits the compare on the common
	// "obviously different" case; the constant-time compare guards
	// against side-channel timing leaks on the equal-length case.
	if len(got) != len(expectedCmd) {
		return got, ErrTrustRebindDiverged
	}
	if subtle.ConstantTimeCompare(got, expectedCmd) != 1 {
		return got, ErrTrustRebindDiverged
	}
	return got, nil
}
