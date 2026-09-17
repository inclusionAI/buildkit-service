//go:build !linux || !cgo

package buildbatch

import "errors"

type lmdbResultDB struct{}

func openLMDBResultDB(path string) (*lmdbResultDB, error) {
	return nil, errors.New("lmdb support requires linux with cgo enabled")
}

func (db *lmdbResultDB) Put(entry resultEntry) error {
	return errors.New("lmdb support requires linux with cgo enabled")
}

func (db *lmdbResultDB) Get(target string) (resultEntry, bool, error) {
	return resultEntry{}, false, errors.New("lmdb support requires linux with cgo enabled")
}

func (db *lmdbResultDB) All() ([]resultEntry, error) {
	return nil, errors.New("lmdb support requires linux with cgo enabled")
}

func (db *lmdbResultDB) Sync() error {
	return errors.New("lmdb support requires linux with cgo enabled")
}

func (db *lmdbResultDB) Close() error {
	return nil
}
