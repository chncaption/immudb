package store

import (
	"errors"
	"fmt"

	"github.com/codenotary/immudb/embedded/tbtree"
)

type KVConstraints struct {
	MustExist          bool
	MustNotExist       bool
	NotModifiedAfterTX uint64
}

func (cs *KVConstraints) validate() error {
	if !cs.MustExist && !cs.MustNotExist && cs.NotModifiedAfterTX == 0 {
		return fmt.Errorf("%w: no constraint was set", ErrInvalidConstraint)
	}
	if cs.MustExist && cs.MustNotExist {
		return fmt.Errorf("%w: conflicting MustExist and MustNotExist constraints", ErrInvalidConstraint)
	}
	return nil
}

func (cs *KVConstraints) check(key []byte, snap *tbtree.Snapshot) error {
	_, tx, _, err := snap.Get(key)
	if err != nil && !errors.Is(err, tbtree.ErrKeyNotFound) {
		return fmt.Errorf("couldn't check KV constraint: %w", err)
	}

	if cs.MustExist && err != nil {
		return fmt.Errorf("%w: key does not exist", ErrConstraintFailed)
	}

	if cs.MustNotExist && err == nil {
		return fmt.Errorf("%w: key already exists", ErrConstraintFailed)
	}

	if cs.NotModifiedAfterTX > 0 && tx > cs.NotModifiedAfterTX {
		return fmt.Errorf("%w: key modified after given TX", ErrConstraintFailed)
	}

	return nil
}
