// sse.go — Server-Sent Events emitter shared by the setup wizard and the
// /backup admin section.
//
// Both the wizard's finalize pipeline and the /backup create+restore
// pipelines stream long-running progress to the browser as SSE frames.
// They emit four event types — `status`, `log`, `error`, `done` — using
// the exact same JSON envelopes. Extracted from the original inline
// closures in setup.go so the second consumer (backup.go) can reuse the
// emitter without copying the JSON shape into a second location and
// drifting.
//
// Frame format (all four events): `data:` is a single JSON object on one
// line; `event:` selects the EventSource handler in the browser:
//
//   event: status
//   data: {"step":"users_db","msg":"writing admin to users_database.yml"}
//
//   event: log
//   data: {"line":"+ docker compose up -d"}
//
//   event: error
//   data: {"step":"cert","msg":"certbot exited 1: <last lines>"}
//
//   event: done
//   data: {"redirect":"https://example.com/"}
//
// Concurrency: writes are serialised through the supplied writeFrame
// callback. The intended caller is the SSE handler which already holds a
// per-connection writeMu so a keepalive `: keepalive\n\n` from a separate
// goroutine cannot interleave bytes into a half-written `data:` frame.

package main

import "fmt"

// sseEmitter is a small bundle of typed emit helpers built around a
// frame-writer closure. Construct one per SSE connection via
// newSSEEmitter and pass it through the long-running pipeline. None of
// the methods is safe for concurrent calls without the underlying
// writeFrame doing its own locking — same contract as the original
// setup.go closures.
type sseEmitter struct {
	// writeFrame writes one full SSE frame (including the trailing
	// blank line) and flushes the response. The handler's keepalive
	// goroutine MUST share the same lock with writeFrame to prevent
	// `: keepalive\n\n` from being interleaved into a half-emitted
	// `data:` line.
	writeFrame func([]byte)
}

// newSSEEmitter wraps a writeFrame callback. Returns by value so callers
// don't have to nil-check.
func newSSEEmitter(writeFrame func([]byte)) sseEmitter {
	return sseEmitter{writeFrame: writeFrame}
}

// emit is the low-level escape hatch — every payload is sent as-is and
// MUST already be a single line of valid JSON (or whatever the consumer
// accepts on the named event). Prefer Status/Log/Error/Done.
func (e sseEmitter) emit(event, payload string) {
	e.writeFrame([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload)))
}

// Status emits `event: status` with `{"step":<step>,"msg":<msg>}`. Use
// for "moved to a new pipeline phase" announcements; the browser surfaces
// them as a high-level progress meter step.
func (e sseEmitter) Status(step, msg string) {
	e.emit("status", fmt.Sprintf(`{"step":%q,"msg":%q}`, step, msg))
}

// Log emits `event: log` with `{"line":<line>}`. Use for free-form output
// the operator should see scrolling — typically captured stdout/stderr
// from a shelled-out command. The caller does not need to escape the
// line; %q does the JSON-string escaping.
func (e sseEmitter) Log(line string) {
	e.emit("log", fmt.Sprintf(`{"line":%q}`, line))
}

// Error emits `event: error` with `{"step":<step>,"msg":<msg>}`. The
// browser interprets this as terminal failure for the current pipeline
// and stops listening for further events.
func (e sseEmitter) Error(step, msg string) {
	e.emit("error", fmt.Sprintf(`{"step":%q,"msg":%q}`, step, msg))
}

// Done emits `event: done` with the supplied JSON payload (e.g.
// `{"redirect":"https://..."}`). Caller is responsible for the JSON
// shape so this method is the rare one that doesn't construct an
// envelope itself.
func (e sseEmitter) Done(payloadJSON string) {
	e.emit("done", payloadJSON)
}
