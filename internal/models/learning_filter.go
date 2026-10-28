package models

// RecallableLearningSQL is the SQL predicate every read path over the learnings
// table must apply before handing rows to a caller: a superseded learning is
// replaced by its successor, and a quarantined one is suspended until review.
// Both are unreachable by design, not by accident.
//
// COALESCE instead of a bare "IS NULL" puts an empty-string marker in the same
// bucket as NULL. No writer stores "" today, but two older queries in this
// package already read the column that way; a single spelling avoids a row
// becoming invisible for the wrong reason.
const RecallableLearningSQL = `superseded_by IS NULL AND COALESCE(quarantined_at, '') = ''`

// RecallableLearningSQLAlias is RecallableLearningSQL written for queries that
// reference the learnings table under the alias "l".
const RecallableLearningSQLAlias = `l.superseded_by IS NULL AND COALESCE(l.quarantined_at, '') = ''`
