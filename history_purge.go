package assistant

import (
	"context"
	"errors"

	"github.com/redelay/go-flowdsl/flowexec/storage-mongo/sink/mongots"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
)

// Erasing the second copy of a conversation.
//
// A chat's text lives in two places and only one of them is the transcript:
//
//   - `assistant_chats` — the conversation, TTL'd, what the widget reads back;
//   - `flow_events.payload` — every node's input and output packet, written by
//     the flowexec executor, which for a conversation-carrying flow contains
//     the same messages (and the composed system prompt, and the reply) once
//     per node per turn.
//
// Nothing in this kernel used to touch the second one, and nothing pointed at
// it: a reader looking for "where do we keep chat content" finds `chats` and
// stops. These helpers exist so every delete path in this package has to pass
// through the question.
//
// Mongo-only by construction. The run history is a time-series collection
// whose erasure rules are specific to it (see mongots.PurgeUser), and a
// deployment with another sink has its own answer; `m.db == nil` means no
// persistence at all, hence nothing to purge.

// ErrNoHistoryStore is returned when run history cannot be reached, so a
// caller can log the run ids for a later sweep rather than assume success.
var ErrNoHistoryStore = errors.New("assistant: no Mongo run-history store")

// purgeRunHistory deletes the flow run history of specific runs.
func (m *Module) purgeRunHistory(ctx context.Context, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	if m.db == nil {
		return ErrNoHistoryStore
	}
	res, err := mongots.PurgeRuns(ctx, m.db, mongots.Config{}, runIDs)
	if err != nil {
		return err
	}
	m.logger.Info("assistant: run history purged",
		zap.Int("runs", res.Runs),
		zap.Int64("flow_events", res.FlowEvents),
		zap.Int64("flow_runs", res.FlowRuns),
		zap.Int64("llm_usage", res.LLMUsage))
	return nil
}

// PurgeMemberHistory deletes every chat a member has and all the flow run
// history behind it, returning whether anything naming them is left.
//
// Exported for a consuming app's account-deletion sweep: the kernel owns the
// chat collection and the knowledge that its text is duplicated into the run
// history, so the app should not have to know either.
//
// `clean` is false when the verification could not be done OR found rows
// still present — a caller must not stamp "erased" on a value it did not get.
func (m *Module) PurgeMemberHistory(ctx context.Context, userID primitive.ObjectID) (chatsDeleted int, clean bool, err error) {
	if m.db == nil || userID.IsZero() {
		return 0, false, ErrNoHistoryStore
	}

	// The transcript first, but READ before deleted: the run ids are inside
	// the documents, and after a DeleteMany there is nothing left to read
	// them from.
	var runIDs []string
	if m.chats != nil {
		runIDs, err = m.chats.DeleteForUser(ctx, userID)
		if err != nil {
			return 0, false, err
		}
		chatsDeleted = len(runIDs)
	}

	// By user id, which also reaches runs whose transcript had already
	// expired on the chat TTL, and the pre-attribution backlog by scan.
	res, perr := mongots.PurgeUser(ctx, m.db, mongots.Config{}, userID.Hex())
	if perr != nil {
		return chatsDeleted, false, perr
	}
	// And by run id, for runs the chats named but whose own summary row is
	// already gone, so PurgeUser could not resolve them.
	if len(runIDs) > 0 {
		if _, err := mongots.PurgeRuns(ctx, m.db, mongots.Config{}, runIDs); err != nil {
			return chatsDeleted, false, err
		}
	}

	m.logger.Info("assistant: member history purged",
		zap.String("user_id", userID.Hex()),
		zap.Int("chats", chatsDeleted),
		zap.Int("runs", res.Runs),
		zap.Int("runs_from_scan", res.RunsFromScan),
		zap.Int64("flow_events", res.FlowEvents),
		zap.Int64("remaining", res.Remaining))
	return chatsDeleted, res.Clean(), nil
}
