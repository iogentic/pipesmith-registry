package catalog

import (
	"database/sql"
	"errors"
)

// AssetDetail is what catalog.sqlite holds of one blob beyond the index,
// for the REST asset answer: uploader and address as the properties file
// names them, the digests a write computed ("" for unknown), and the last
// modified time of the asset while the blob wins it.
type AssetDetail struct {
	Uploader, Address   *string
	MD5, SHA256, SHA512 string
	LastModifiedMS      *int64
}

// Detail is the side data of the blob store holds for a path, when it is
// the blob created at createdMS, the one the index serves; nil when the
// catalog holds another blob of the path or none.
func (c *Catalog) Detail(repo, path, store string, createdMS int64) (*AssetDetail, error) {
	var d *AssetDetail
	err := c.read(func(tx *sql.Tx) error {
		var created int64
		var uploader, address, md5, sha256, sha512, winner sql.NullString
		var modified sql.NullInt64
		err := tx.QueryRowContext(bg, "SELECT b.created_ms, b.uploader, b.uploader_ip, b.md5, b.sha256, b.sha512, a.store,"+
			" a.last_modified_ms FROM blobs b LEFT JOIN assets a ON a.repo = b.repo AND a.path = b.path"+
			" WHERE b.store = ? AND b.repo = ? AND b.path = ?", store, repo, path).Scan(&created, &uploader, &address, &md5,
			&sha256, &sha512, &winner, &modified)
		if errors.Is(err, sql.ErrNoRows) || err == nil && created != createdMS {
			return nil
		}
		if err != nil {
			return err
		}
		d = &AssetDetail{MD5: md5.String, SHA256: sha256.String, SHA512: sha512.String}
		if uploader.Valid {
			d.Uploader = &uploader.String
		}
		if address.Valid {
			d.Address = &address.String
		}
		if modified.Valid && winner.String == store {
			d.LastModifiedMS = &modified.Int64
		}
		return nil
	})
	return d, err
}
