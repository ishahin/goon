/*
 * Copyright (c) 2012 Matt Jibson <matt.jibson@gmail.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package goon

import (
	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"bytes"
	"encoding/gob"
	"errors"
	"net/http"
	"reflect"
)

// Goon holds the app engine context and request memory cache.
type Goon struct {
	context       appengine.Context
	cache         map[string]*Entity
	inTransaction bool
	toSet         map[string]*Entity
	toDelete      map[string]*Entity
}

func memkey(k *datastore.Key) string {
	return k.String()
}

func NewGoon(r *http.Request) *Goon {
	return &Goon{
		context: appengine.NewContext(r),
		cache:   make(map[string]*Entity),
	}
}

// RunInTransaction runs f in a transaction. It calls f with a transaction
// context g that f should use for all App Engine operations. Neither cache nor
// memcache are used or set during a transaction.
//
// Otherwise similar to appengine/datastore.RunInTransaction:
// https://developers.google.com/appengine/docs/go/datastore/reference#RunInTransaction
func (g *Goon) RunInTransaction(f func(g *Goon) error, opts *datastore.TransactionOptions) error {
	var ng *Goon
	err := datastore.RunInTransaction(g.context, func(tc appengine.Context) error {
		ng = &Goon{
			context:       tc,
			inTransaction: true,
			toSet:         make(map[string]*Entity),
			toDelete:      make(map[string]*Entity),
		}
		return f(ng)
	}, opts)

	if err == nil {
		for k, v := range ng.toSet {
			g.cache[k] = v
		}

		for k := range ng.toDelete {
			delete(g.cache, k)
		}
	}

	return err
}

// Put stores Entity e.
// If e has an incomplete key, it is updated.
func (g *Goon) Put(e *Entity) error {
	return g.PutMulti([]*Entity{e})
}

// PutMulti stores a sequence of Entities.
// Any entity with an incomplete key will be updated.
func (g *Goon) PutMulti(es []*Entity) error {
	var err error

	var memkeys []string
	keys := make([]*datastore.Key, len(es))
	src := make([]interface{}, len(es))

	for i, e := range es {
		if !e.Key.Incomplete() {
			memkeys = append(memkeys, e.memkey())
		}

		keys[i] = e.Key
		src[i] = e.Src
	}

	memcache.DeleteMulti(g.context, memkeys)

	keys, err = datastore.PutMulti(g.context, keys, src)

	if err != nil {
		return err
	}

	for i, e := range es {
		es[i].setKey(keys[i])

		if g.inTransaction {
			g.toSet[e.memkey()] = e
		}
	}

	if !g.inTransaction {
		g.putMemoryMulti(es)
	}

	return nil
}

func (g *Goon) putMemoryMulti(es []*Entity) {
	for _, e := range es {
		g.putMemory(e)
	}
}

func (g *Goon) putMemory(e *Entity) {
	g.cache[e.memkey()] = e
}

func (g *Goon) putMemcache(es []*Entity) error {
	items := make([]*memcache.Item, len(es))

	for i, e := range es {
		gob, err := e.gob()
		if err != nil {
			return err
		}

		items[i] = &memcache.Item{
			Key:   e.memkey(),
			Value: gob,
		}
	}

	err := memcache.SetMulti(g.context, items)

	if err != nil {
		return err
	}

	g.putMemoryMulti(es)
	return nil
}

// structKind returns the reflect.Kind name of src if it is a struct, else nil.
func structKind(src interface{}) (string, error) {
	v := reflect.ValueOf(src)
	v = reflect.Indirect(v)
	t := v.Type()
	k := t.Kind()

	if k == reflect.Struct {
		return t.Name(), nil
	}
	return "", errors.New("goon: src has invalid type")
}

// Get fetches an entity of kind src by.
// Refer to appengine/datastore.NewKey regarding key specification.
func (g *Goon) Get(src interface{}, stringID string, intID int64, parent *datastore.Key) (*Entity, error) {
	k, err := structKind(src)
	if err != nil {
		return nil, err
	}
	key := datastore.NewKey(g.context, k, stringID, intID, parent)
	return g.KeyGet(src, key)
}

// KeyGet fetches an entity of kind src by key.
func (g *Goon) KeyGet(src interface{}, key *datastore.Key) (*Entity, error) {
	e := NewEntity(key, src)
	es := []*Entity{e}
	err := g.GetMulti(es)
	if err != nil {
		return nil, err
	}
	return es[0], nil
}

// Get fetches a sequency of Entities, whose keys must already be valid.
// Entities with no correspending key have their NotFound field set to true.
func (g *Goon) GetMulti(es []*Entity) error {
	var dskeys []*datastore.Key
	var dst []interface{}
	var dixs []int

	if !g.inTransaction {
		var memkeys []string
		var mixs []int

		for i, e := range es {
			m := e.memkey()
			if s, present := g.cache[m]; present {
				es[i] = s
			} else {
				memkeys = append(memkeys, m)
				mixs = append(mixs, i)
			}
		}

		memvalues, err := memcache.GetMulti(g.context, memkeys)
		if err != nil {
			return err
		}

		for i, m := range memkeys {
			e := es[mixs[i]]
			if s, present := memvalues[m]; present {
				err := fromGob(e, s.Value)
				if err != nil {
					return err
				}

				g.putMemory(e)
			} else {
				dskeys = append(dskeys, e.Key)
				dst = append(dst, e.Src)
				dixs = append(dixs, mixs[i])
			}
		}
	} else {
		dskeys = make([]*datastore.Key, len(es))
		dst = make([]interface{}, len(es))
		dixs = make([]int, len(es))

		for i, e := range es {
			dskeys[i] = e.Key
			dst[i] = e.Src
			dixs[i] = i
		}
	}

	var merr appengine.MultiError
	err := datastore.GetMulti(g.context, dskeys, dst)
	if err != nil {
		merr = err.(appengine.MultiError)
	}
	var mes []*Entity

	for i, idx := range dixs {
		e := es[idx]
		if merr != nil && merr[i] != nil {
			e.NotFound = true
		}
		mes = append(mes, e)
	}

	if !g.inTransaction {
		err = g.putMemcache(mes)
		if err != nil {
			return err
		}
	}

	multiErr, any := make(appengine.MultiError, len(es)), false
	for i, e := range es {
		if e.NotFound {
			multiErr[i] = datastore.ErrNoSuchEntity
			any = true
		}
	}

	if any {
		return multiErr
	}

	return nil
}

func fromGob(e *Entity, b []byte) error {
	var buf bytes.Buffer
	_, _ = buf.Write(b)
	gob.Register(e.Src)
	dec := gob.NewDecoder(&buf)
	return dec.Decode(e)
}
