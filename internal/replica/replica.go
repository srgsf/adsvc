// Package replica syncs catalogues between federated peers (docs/database.md §8): every
// node serves its log (/sync/*), pulls the logs of the peers it is configured with, and
// pushes its own to peers that cannot pull from it (a TV box behind NAT).
//
// Records travel in the binary frames of package record; each is checked by the receiver
// (catalog.Ingest) as if nobody could be trusted, so the transport needs no more than
// knowing which node a batch came from.
package replica

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/record"
)

// Content types of the sync endpoints.
const (
	typeLog     = "application/x-adsvc-log"     // (uvarint id, record frame)*
	typeRecords = "application/x-adsvc-records" // record frame*
)

// Info is what GET /sync/info returns.
type Info struct {
	NodeID    record.Origin `json:"node_id"`
	Name      string        `json:"name,omitempty"`
	FPVersion int           `json:"fp_version"`
	Schema    int           `json:"schema"`
	Head      int64         `json:"head"` // id of the last record in the log
	// Sig signs the nonce the caller sent, with NodeID: the node is who it says it is, so
	// records pulled from it are attributed to it.
	Sig []byte `json:"sig"`
}

// Header names of a signed push.
const (
	hdrNode = "X-Adsvc-Node"
	hdrDate = "X-Adsvc-Date"
	hdrSig  = "X-Adsvc-Signature"
)

// maxPushAge is how old (or how far ahead) a signed push may be.
const maxPushAge = 10 * time.Minute

func infoMessage(nonce []byte, node record.Origin) []byte {
	return bytes.Join([][]byte{[]byte("adsvc sync info"), nonce, node[:]}, []byte{0})
}

func pushMessage(date string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return bytes.Join([][]byte{[]byte("adsvc sync push"), []byte(date), sum[:]}, []byte{0})
}

func newNonce() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand does not fail
	return b
}

// appendEntry appends one log entry: its id, then the record frame.
func appendEntry(dst []byte, e *catalog.Entry) []byte {
	dst = binary.AppendUvarint(dst, uint64(e.ID))
	return record.AppendFrame(dst, &e.Record)
}

// readEntries reads a log response.
func readEntries(r io.Reader, max int) ([]catalog.Entry, error) {
	br := bufio.NewReader(r)
	var out []catalog.Entry
	for len(out) <= max {
		id, err := binary.ReadUvarint(br)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("log entry: %w", err)
		}
		rec, err := record.ReadFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return out, fmt.Errorf("log entry %d: %w", id, err)
		}
		if n := len(out); id > 1<<62 || (n > 0 && int64(id) <= out[n-1].ID) {
			return out, fmt.Errorf("log entry %d out of order", id)
		}
		out = append(out, catalog.Entry{ID: int64(id), Record: rec})
	}
	return out, fmt.Errorf("more than the %d entries asked for", max)
}

// readRecords reads a push body (at most max records).
func readRecords(r io.Reader, max int) ([]record.Record, error) {
	br := bufio.NewReader(r)
	var out []record.Record
	for {
		rec, err := record.ReadFrame(br)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if len(out) == max {
			return out, fmt.Errorf("more than %d records", max)
		}
		out = append(out, rec)
	}
}

// ErrNotPeer is a sync endpoint answering as another node than the one it said it was.
var ErrNotPeer = errors.New("replica: peer's signature does not match its node id")

func parseDate(s string, now time.Time) error {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("bad %s", hdrDate)
	}
	if d := now.Sub(time.UnixMilli(ms)); d > maxPushAge || d < -maxPushAge {
		return fmt.Errorf("%s is %s off", hdrDate, d.Round(time.Second))
	}
	return nil
}

func hexID(o record.Origin) string { return hex.EncodeToString(o[:]) }
