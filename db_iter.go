// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"

	"github.com/petermattis/pebble/db"
)

type dbIterPos int8

const (
	dbIterCur  dbIterPos = 0
	dbIterNext dbIterPos = 1
	dbIterPrev dbIterPos = -1
)

// Iterator iterates over a DB's key/value pairs in key order.
//
// An iterator must be closed after use, but it is not necessary to read an
// iterator until exhaustion.
//
// An iterator is not necessarily goroutine-safe, but it is safe to use
// multiple iterators concurrently, with each in a dedicated goroutine.
//
// It is also safe to use an iterator concurrently with modifying its
// underlying DB, if that DB permits modification. However, the resultant
// key/value pairs are not guaranteed to be a consistent snapshot of that DB
// at a particular point in time.
type dbIter struct {
	opts      *db.IterOptions
	cmp       db.Compare
	merge     db.Merge
	iter      internalIterator
	version   *version
	err       error
	key       []byte
	keyBuf    []byte
	value     []byte
	valueBuf  []byte
	valueBuf2 []byte
	valid     bool
	pos       dbIterPos
}

var _ Iterator = (*dbIter)(nil)

func (i *dbIter) findNextEntry() bool {
	upperBound := i.opts.GetUpperBound()
	i.valid = false
	i.pos = dbIterCur

	for i.iter.Valid() {
		key := i.iter.Key()
		if upperBound != nil && i.cmp(key.UserKey, upperBound) >= 0 {
			break
		}

		switch key.Kind() {
		case db.InternalKeyKindDelete:
			i.nextUserKey()
			continue

		case db.InternalKeyKindRangeDelete:
			// Range deletions are treated as no-ops. See the comments in levelIter
			// for more details.
			i.iter.Next()
			continue

		case db.InternalKeyKindSet:
			i.keyBuf = append(i.keyBuf[:0], key.UserKey...)
			i.key = i.keyBuf
			i.value = i.iter.Value()
			i.valid = true
			return true

		case db.InternalKeyKindMerge:
			return i.mergeNext(key)

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", key.Kind())
			return false
		}
	}

	return false
}

func (i *dbIter) nextUserKey() {
	if i.iter.Valid() {
		if !i.valid {
			i.keyBuf = append(i.keyBuf[:0], i.iter.Key().UserKey...)
			i.key = i.keyBuf
		}
		for i.iter.Next() {
			if i.cmp(i.key, i.iter.Key().UserKey) != 0 {
				break
			}
		}
	} else {
		i.iter.First()
	}
}

func (i *dbIter) findPrevEntry() bool {
	lowerBound := i.opts.GetLowerBound()
	i.valid = false
	i.pos = dbIterCur

	for i.iter.Valid() {
		key := i.iter.Key()
		if lowerBound != nil && i.cmp(key.UserKey, lowerBound) < 0 {
			break
		}

		if i.valid {
			if i.cmp(key.UserKey, i.key) < 0 {
				// We've iterated to the previous user key.
				i.pos = dbIterPrev
				return true
			}
		}

		switch key.Kind() {
		case db.InternalKeyKindDelete:
			i.value = nil
			i.valid = false
			i.iter.Prev()
			continue

		case db.InternalKeyKindRangeDelete:
			// Range deletions are treated as no-ops. See the comments in levelIter
			// for more details.
			i.iter.Prev()
			continue

		case db.InternalKeyKindSet:
			i.keyBuf = append(i.keyBuf[:0], key.UserKey...)
			i.key = i.keyBuf
			i.value = i.iter.Value()
			i.valid = true
			i.iter.Prev()
			continue

		case db.InternalKeyKindMerge:
			if !i.valid {
				i.keyBuf = append(i.keyBuf[:0], key.UserKey...)
				i.key = i.keyBuf
				i.value = i.iter.Value()
				i.valid = true
			} else {
				// The existing value is either stored in valueBuf2 or the underlying
				// iterators value. We append the new value to valueBuf in order to
				// merge(valueBuf, valueBuf2). Then we swap valueBuf and valueBuf2 in
				// order to maintain the invariant that the existing value points to
				// valueBuf2 (in preparation for handling th next merge value).
				i.valueBuf = append(i.valueBuf[:0], i.iter.Value()...)
				i.valueBuf = i.merge(i.key, i.valueBuf, i.value, nil)
				i.valueBuf, i.valueBuf2 = i.valueBuf2, i.valueBuf
				i.value = i.valueBuf2
			}
			i.iter.Prev()
			continue

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", key.Kind())
			return false
		}
	}

	if i.valid {
		i.pos = dbIterPrev
		return true
	}

	return false
}

func (i *dbIter) prevUserKey() {
	if i.iter.Valid() {
		if !i.valid {
			i.keyBuf = append(i.keyBuf[:0], i.iter.Key().UserKey...)
			i.key = i.keyBuf
		}
		for i.iter.Prev() {
			if i.cmp(i.key, i.iter.Key().UserKey) != 0 {
				break
			}
		}
	} else {
		i.iter.Last()
	}
}

func (i *dbIter) mergeNext(key db.InternalKey) bool {
	// Save the current key and value.
	i.keyBuf = append(i.keyBuf[:0], key.UserKey...)
	i.valueBuf = append(i.valueBuf[:0], i.iter.Value()...)
	i.key, i.value = i.keyBuf, i.valueBuf
	i.valid = true

	// Loop looking for older values for this key and merging them.
	for {
		i.iter.Next()
		if !i.iter.Valid() {
			i.pos = dbIterNext
			return true
		}
		key = i.iter.Key()
		if i.cmp(i.key, key.UserKey) != 0 {
			// We've advanced to the next key.
			i.pos = dbIterNext
			return true
		}
		switch key.Kind() {
		case db.InternalKeyKindDelete:
			// We've hit a deletion tombstone. Return everything up to this
			// point.
			return true

		case db.InternalKeyKindRangeDelete:
			// Range deletions are treated as no-ops. See the comments in levelIter
			// for more details.
			continue

		case db.InternalKeyKindSet:
			// We've hit a Set value. Merge with the existing value and return.
			i.value = i.merge(i.key, i.value, i.iter.Value(), nil)
			return true

		case db.InternalKeyKindMerge:
			// We've hit another Merge value. Merge with the existing value and
			// continue looping.
			i.value = i.merge(i.key, i.value, i.iter.Value(), nil)
			i.valueBuf = i.value[:0]
			continue

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", key.Kind())
			return false
		}
	}
}

// SeekGE moves the iterator to the first key/value pair whose key is greater
// than or equal to the given key.
func (i *dbIter) SeekGE(key []byte) {
	if i.err != nil {
		return
	}

	if lowerBound := i.opts.GetLowerBound(); lowerBound != nil && i.cmp(key, lowerBound) < 0 {
		key = lowerBound
	}

	i.iter.SeekGE(key)
	i.findNextEntry()
}

// SeekLT moves the iterator to the last key/value pair whose key is less than
// the given key.
func (i *dbIter) SeekLT(key []byte) {
	if i.err != nil {
		return
	}

	if upperBound := i.opts.GetUpperBound(); upperBound != nil && i.cmp(key, upperBound) >= 0 {
		key = upperBound
	}

	i.iter.SeekLT(key)
	i.findPrevEntry()
}

// First moves the iterator the the first key/value pair.
func (i *dbIter) First() {
	if i.err != nil {
		return
	}

	if lowerBound := i.opts.GetLowerBound(); lowerBound != nil {
		i.SeekGE(lowerBound)
		return
	}

	i.iter.First()
	i.findNextEntry()
}

// Last moves the iterator the the last key/value pair.
func (i *dbIter) Last() {
	if i.err != nil {
		return
	}

	if upperBound := i.opts.GetUpperBound(); upperBound != nil {
		i.SeekLT(upperBound)
		return
	}

	i.iter.Last()
	i.findPrevEntry()
}

// Next moves the iterator to the next key/value pair.
// It returns whether the iterator is exhausted.
func (i *dbIter) Next() bool {
	if i.err != nil {
		return false
	}
	switch i.pos {
	case dbIterCur:
		i.nextUserKey()
	case dbIterPrev:
		i.nextUserKey()
		i.nextUserKey()
	case dbIterNext:
	}
	return i.findNextEntry()
}

// Prev moves the iterator to the previous key/value pair.
// It returns whether the iterator is exhausted.
func (i *dbIter) Prev() bool {
	if i.err != nil {
		return false
	}
	switch i.pos {
	case dbIterCur:
		i.prevUserKey()
	case dbIterNext:
		i.prevUserKey()
		i.prevUserKey()
	case dbIterPrev:
	}
	return i.findPrevEntry()
}

// Key returns the key of the current key/value pair, or nil if done. The
// caller should not modify the contents of the returned slice, and its
// contents may change on the next call to Next.
func (i *dbIter) Key() []byte {
	return i.key
}

// Value returns the value of the current key/value pair, or nil if done. The
// caller should not modify the contents of the returned slice, and its
// contents may change on the next call to Next.
func (i *dbIter) Value() []byte {
	return i.value
}

// Valid returns true if the iterator is positioned at a valid key/value pair
// and false otherwise.
func (i *dbIter) Valid() bool {
	return i.valid
}

// Error returns any accumulated error.
func (i *dbIter) Error() error {
	return i.err
}

// Close closes the iterator and returns any accumulated error. Exhausting
// all the key/value pairs in a table is not considered to be an error.
// It is valid to call Close multiple times. Other methods should not be
// called after the iterator has been closed.
func (i *dbIter) Close() error {
	if i.version != nil {
		i.version.unref()
		i.version = nil
	}
	if err := i.iter.Close(); err != nil && i.err != nil {
		i.err = err
	}
	return i.err
}
