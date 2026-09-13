package audit

import (
	"database/sql"
	"log"
)

type Recorder struct{ DB *sql.DB }

func New(d *sql.DB) *Recorder { return &Recorder{DB: d} }

// Record 写入审计日志；写入失败只记录日志、不中断业务请求（签名保持稳定，
// 调用方无需处理错误）。
func (r *Recorder) Record(userID int64, username string, instanceID int64, action, detail, ip string) {
	if instanceID == 0 {
		if _, err := r.DB.Exec(`INSERT INTO audit_logs(user_id, username, instance_id, action, detail, ip)
			VALUES(?,?,NULL,?,?,?)`, userID, username, action, detail, ip); err != nil {
			log.Printf("[audit] write failed: %v", err)
		}
		return
	}
	if _, err := r.DB.Exec(`INSERT INTO audit_logs(user_id, username, instance_id, action, detail, ip)
		VALUES(?,?,?,?,?,?)`, userID, username, instanceID, action, detail, ip); err != nil {
		log.Printf("[audit] write failed: %v", err)
	}
}
