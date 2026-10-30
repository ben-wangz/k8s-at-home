package workqueue

import "encoding/json"

type Checkpoint struct {
	Phase            int     `json:"phase"`
	Offset           int     `json:"offset"`
	Index            int     `json:"index"`
	SessionOffset    int     `json:"sessionOffset"`
	Processed        uint64  `json:"processed"`
	ConfirmedTargets uint64  `json:"confirmedTargets"`
	PagePosition     int     `json:"pagePosition"`
	BufferedEntities int     `json:"bufferedEntities"`
	ScanComplete     bool    `json:"scanComplete"`
	HasContinuation  bool    `json:"hasContinuation"`
	Total            *uint64 `json:"total"`
	Unit             string  `json:"unit"`
}

func checkpointSummary(data json.RawMessage) Checkpoint {
	var value struct {
		Phase, Offset, Index, SessionOffset int
		ConfirmedTargets                    uint64
		Token                               string
		Cursor                              struct {
			Entities    uint64
			Offset      int
			Entries     []json.RawMessage
			Token, Next string
			Done        bool
		}
	}
	_ = json.Unmarshal(data, &value)
	c := value.Cursor
	return Checkpoint{Phase: value.Phase, Offset: value.Offset, Index: value.Index, SessionOffset: value.SessionOffset, ConfirmedTargets: value.ConfirmedTargets, Processed: c.Entities, PagePosition: c.Offset, BufferedEntities: max(0, len(c.Entries)-c.Offset), ScanComplete: c.Done, HasContinuation: c.Token != "" || c.Next != "" || value.Token != "", Unit: "source_entities; total unknown until scan completion"}
}
