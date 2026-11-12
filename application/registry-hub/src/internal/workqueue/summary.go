package workqueue

import (
	"encoding/json"
	"registry-hub/internal/storage"
)

const MaxFinalSummaryBytes = 8 << 10

type CompletionSummarizer interface {
	CompletionSummary(Job) (json.RawMessage, error)
}

func validFinalSummary(data json.RawMessage) bool {
	return len(data) > 0 && len(data) <= MaxFinalSummaryBytes && json.Valid(data) && data[0] == '{'
}

func (q Queue) summarize(job *Job) error {
	if len(job.FinalSummary) == 0 && q.BuildFinalSummary != nil {
		data, err := q.BuildFinalSummary(*job)
		if err != nil {
			return err
		}
		job.FinalSummary = data
	}
	if len(job.FinalSummary) > 0 && !validFinalSummary(job.FinalSummary) {
		return storage.ErrCorruptObject
	}
	return nil
}
