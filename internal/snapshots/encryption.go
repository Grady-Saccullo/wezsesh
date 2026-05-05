// Encryption magic-byte sniff (Appendix B). Looks at the first 32 bytes
// of a snapshot file and classifies the storage format. Operations that
// do not need to decrypt (switch, save-overwrite, rename, delete, tag,
// pin) work uniformly on every classification; only preview is degraded
// for non-plaintext snapshots.
//
// Whitespace is treated as plaintext-JSON-leading because resurrect's
// own writer can emit a leading newline in some shapes; doctor warns
// when any snapshot is non-JSON.
package snapshots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Grady-Saccullo/wezsesh/internal/safefs"
)

// Encryption is the magic-byte classification for a snapshot file.
type Encryption int

const (
	EncryptionPlaintext Encryption = iota
	EncryptionAge
	EncryptionOpenPGP
	EncryptionUnknown
)

// String renders the enum for debug + doctor output.
func (e Encryption) String() string {
	switch e {
	case EncryptionPlaintext:
		return "plaintext"
	case EncryptionAge:
		return "age"
	case EncryptionOpenPGP:
		return "openpgp"
	case EncryptionUnknown:
		return "unknown"
	}
	return fmt.Sprintf("Encryption(%d)", int(e))
}

// ageMagic is the literal header age (and rage) write at the top of an
// encrypted file. Per https://age-encryption.org/v1.
var ageMagic = []byte("age-encryption.org/v1\n")

// SniffBytes classifies a buffer per Appendix B. Up to 32 bytes are
// inspected; the buffer can be shorter (a 1-byte file is a legal input).
func SniffBytes(b []byte) Encryption {
	if len(b) == 0 {
		// Empty file — neither plaintext JSON nor a valid header. Mark
		// unknown so the caller can degrade preview but still permit
		// rename/delete.
		return EncryptionUnknown
	}
	if bytes.HasPrefix(b, ageMagic) {
		return EncryptionAge
	}
	// OpenPGP packet tag: high bit MUST be set on the first byte
	// (RFC 4880 §4.2). Both the legacy and new packet formats put the
	// first byte at >= 0x80.
	if b[0]&0x80 != 0 {
		return EncryptionOpenPGP
	}
	switch b[0] {
	case '{', '[':
		return EncryptionPlaintext
	case ' ', '\t', '\n', '\r':
		return EncryptionPlaintext
	}
	return EncryptionUnknown
}

// sniffFile opens path read-only via safefs and sniffs the first 32 bytes.
// Errors are surfaced as-is; missing files return os.ErrNotExist.
func sniffFile(_ context.Context, path string) (Encryption, error) {
	f, err := safefs.SafeOpenForRead(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return EncryptionUnknown, err
		}
		return EncryptionUnknown, err
	}
	defer f.Close()
	var head [32]byte
	n, rerr := f.Read(head[:])
	// io.EOF on a short file is fine; we sniff what we got.
	if n == 0 && rerr != nil {
		return EncryptionUnknown, rerr
	}
	return SniffBytes(head[:n]), nil
}
