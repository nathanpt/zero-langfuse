// Package langfuse builds and posts Langfuse REST ingestion batches.
//
// id.go implements deterministic, idempotent ids (DESIGN §6.3). Trace and score
// resource ids and the ingestion-event id (the dedup key) are UUIDv5 over a
// project namespace; generation/tool span ids are deterministic strings (Langfuse
// accepts arbitrary ids for observations). Determinism means a re-run / retry
// posts the same ids and Langfuse dedupes, so `sync` is idempotent without a
// cursor file.
package langfuse

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
)

// dnsNamespace is the RFC 4122 DNS namespace (6ba7b810-9dad-11d1-80b4-00c04fd430c8).
var dnsNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// namespace is this project's UUIDv5 namespace, derived once from the DNS
// namespace + "zero-langfuse". All resource/event ids derive from it.
var namespace = uuidV5(dnsNamespace, "zero-langfuse")

// uuidV5 computes RFC 4122 version-5 (SHA-1) UUID over (ns, name).
func uuidV5(ns [16]byte, name string) [16]byte {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u
}

// MustUUID formats ns+name as a standard UUID string. It never errors.
func MustUUID(ns [16]byte, name string) string {
	u := uuidV5(ns, name)
	return formatUUID(u)
}

func formatUUID(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// TraceID is the deterministic Langfuse trace resource id for one user turn,
// keyed by (sessionID, openSeq) — the opener message's sequence (0 for an
// implicit opener; DESIGN §6.3).
func TraceID(sessionID string, openSeq int) string {
	return MustUUID(namespace, "trace:"+sessionID+":"+strconv.Itoa(openSeq))
}

// GenerationID is the observation id for one provider_usage event.
func GenerationID(sessionID string, usageSeq int) string {
	return "gen:" + sessionID + ":" + strconv.Itoa(usageSeq)
}

// ToolID is the observation id for one tool_call/tool_result pair.
func ToolID(sessionID, toolCallID string) string {
	return "tool:" + sessionID + ":" + toolCallID
}

// ScoreID is the id for a trace-level score observation.
func ScoreID(traceID, name string) string {
	return "score:" + traceID + ":" + name
}

// EventID is the deterministic ingestion-event id (the dedup key) for a given
// (kind, resourceID). Langfuse dedupes on event.id, so a re-run posting the
// same id is a no-op.
func EventID(kind, resourceID string) string {
	return MustUUID(namespace, "ev:"+kind+":"+resourceID)
}
