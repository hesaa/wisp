package store

// Person is a contact or group known to an account, stored in the people table.
type Person struct {
	JID      string
	PNJID    string
	FullName string
	PushName string
	AvatarID string
	AvatarTS int64
}

// UpsertPeople inserts or updates people for accountID in one transaction.
// A missing row is inserted. A non-empty PNJID, FullName or PushName overwrites the stored value;
// an empty one leaves the stored value intact. The avatar columns are never touched.
func (d *DB) UpsertPeople(accountID string, people []Person) error {
	if len(people) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO people (account_id, jid, pn_jid, full_name, push_name)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(account_id, jid) DO UPDATE SET
			pn_jid = CASE WHEN excluded.pn_jid <> '' THEN excluded.pn_jid ELSE people.pn_jid END,
			full_name = CASE WHEN excluded.full_name <> '' THEN excluded.full_name ELSE people.full_name END,
			push_name = CASE WHEN excluded.push_name <> '' THEN excluded.push_name ELSE people.push_name END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range people {
		if _, err := stmt.Exec(accountID, p.JID, p.PNJID, p.FullName, p.PushName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// People returns all stored people for accountID, ordered by JID.
func (d *DB) People(accountID string) ([]Person, error) {
	rows, err := d.sql.Query(`SELECT jid, pn_jid, full_name, push_name, avatar_id, avatar_ts
		FROM people WHERE account_id = ? ORDER BY jid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.JID, &p.PNJID, &p.FullName, &p.PushName, &p.AvatarID, &p.AvatarTS); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetAvatar upserts the person row for (accountID, jid) and sets both avatar columns.
func (d *DB) SetAvatar(accountID, jid, avatarID string, ts int64) error {
	_, err := d.sql.Exec(`INSERT INTO people (account_id, jid, avatar_id, avatar_ts)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, jid) DO UPDATE SET
			avatar_id = excluded.avatar_id,
			avatar_ts = excluded.avatar_ts`, accountID, jid, avatarID, ts)
	return err
}

// ResetAvatars sets avatar_ts = 0 for every row across all accounts.
func (d *DB) ResetAvatars() error {
	_, err := d.sql.Exec(`UPDATE people SET avatar_ts = 0`)
	return err
}

// ResetEmptyAvatars sets avatar_ts = 0 for rows where avatar_id is empty.
func (d *DB) ResetEmptyAvatars() error {
	_, err := d.sql.Exec(`UPDATE people SET avatar_ts = 0 WHERE avatar_id = ''`)
	return err
}
