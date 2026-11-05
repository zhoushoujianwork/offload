package execution

import "github.com/zhoushoujianwork/offload/internal/transcript"

// Transcript types alias the internal reader types, so their JSON is identical to the output of
// `offload transcript read|list --json`.
type (
	// TranscriptReadOptions selects one invocation (RunID and/or CallerRunID) and a page position.
	TranscriptReadOptions = transcript.ReadOptions
	// TranscriptResponse is one page of a transcript, including availability and reason.
	TranscriptResponse = transcript.Response
	// TranscriptListResponse lists captured transcripts, optionally filtered by caller run ID.
	TranscriptListResponse = transcript.ListResponse
)

// ReadTranscript reads one page of a captured transcript. It only reads the requested run's file;
// it starts no backend and uses no worker slot. A missing run or invalid cursor is reported in the
// response (availability and reason), exactly as the CLI does.
func ReadTranscript(o TranscriptReadOptions) TranscriptResponse { return transcript.Read(o) }

// ListTranscripts lists captured transcripts, newest first. An empty callerRunID lists all; limit 0
// uses the default.
func ListTranscripts(callerRunID string, limit int) TranscriptListResponse {
	return transcript.List(callerRunID, limit)
}
