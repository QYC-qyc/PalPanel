package auth

import "database/sql"

func Seed(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, code := range AllPermissions {
		if _, err := tx.Exec(`INSERT INTO permissions(code) VALUES(?) ON CONFLICT(code) DO NOTHING`, code); err != nil {
			return err
		}
	}
	for _, r := range builtinRoles {
		if _, err := tx.Exec(`INSERT INTO roles(name, description, is_builtin) VALUES(?,?,1)
			ON CONFLICT(name) DO NOTHING`, r.Name, r.Desc); err != nil {
			return err
		}
		var roleID int64
		if err := tx.QueryRow(`SELECT id FROM roles WHERE name=?`, r.Name).Scan(&roleID); err != nil {
			return err
		}
		for _, code := range r.Perms {
			if _, err := tx.Exec(`INSERT INTO role_permissions(role_id, code) VALUES(?,?)
				ON CONFLICT(role_id, code) DO NOTHING`, roleID, code); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
