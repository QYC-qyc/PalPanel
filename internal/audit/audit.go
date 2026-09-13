package audit

import "database/sql"

type Recorder struct{ DB *sql.DB }

func New(d *sql.DB) *Recorder { return &Recorder{DB: d} }

func (r *Recorder) Record(userID int64, username string, instanceID int64, action, detail, ip string) {
	if instanceID == 0 {
		r.DB.Exec(`INSERT INTO audit_logs(user_id, username, instance_id, action, detail, ip)
			VALUES(?,?,NULL,?,?,?)`, userID, username, action, detail, ip)
		return
	}
	r.DB.Exec(`INSERT INTO audit_logs(user_id, username, instance_id, action, detail, ip)
		VALUES(?,?,?,?,?,?)`, userID, username, instanceID, action, detail, ip)
}
