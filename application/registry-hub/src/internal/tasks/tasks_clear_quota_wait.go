package tasks

import "time"

func clearTaskQuotaWait(task *Task) {
	task.QuotaWaitAt = time.Time{}
	task.QuotaWaitReason = ""
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
}
