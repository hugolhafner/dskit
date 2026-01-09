package cache

import (
	"github.com/hugolhafner/dskit/kv"
)

type Client interface {
	kv.Store
}

type clientImpl struct {
	stores []kv.Store
}

func executeOnAllStores[T any](stores []kv.Store, fn func(store kv.Store) (T, bool, error)) (T, error) {
	var zero T
	for _, store := range stores {
		result, exit, err := fn(store)
		if exit {
			return result, err
		}
	}
	return zero, nil
}

func (c clientImpl) Get(key []byte) ([]byte, error) {
	return executeOnAllStores(c.stores, func(store kv.Store) ([]byte, bool, error) {
		value, err := store.Get(key)
		if err != nil {
			return nil, false, err
		}

		if value != nil {
			return value, true, nil
		}

		return nil, false, nil
	})
}

func (c clientImpl) MGet(keys [][]byte) ([][]byte, error) {
	return executeOnAllStores(c.stores, func(store kv.Store) ([][]byte, bool, error) {
		values, err := store.MGet(keys)
		if err != nil {
			return nil, false, err
		}

		allNil := true
		for _, value := range values {
			if value != nil {
				allNil = false
				break
			}
		}

		if !allNil {
			return values, true, nil
		}

		return nil, false, nil
	})
}

func (c clientImpl) Set(key []byte, value []byte) error {
	_, err := executeOnAllStores(c.stores, func(store kv.Store) (struct{}, bool, error) {
		err := store.Set(key, value)
		return struct{}{}, false, err
	})

	return err
}

func (c clientImpl) MSet(keys [][]byte, values [][]byte) error {
	_, err := executeOnAllStores(c.stores, func(store kv.Store) (struct{}, bool, error) {
		err := store.MSet(keys, values)
		return struct{}{}, false, err
	})

	return err
}

func (c clientImpl) Delete(key []byte) error {
	_, err := executeOnAllStores(c.stores, func(store kv.Store) (struct{}, bool, error) {
		err := store.Delete(key)
		return struct{}{}, false, err
	})

	return err
}

func (c clientImpl) MDelete(keys [][]byte) error {
	_, err := executeOnAllStores(c.stores, func(store kv.Store) (struct{}, bool, error) {
		err := store.MDelete(keys)
		return struct{}{}, false, err
	})

	return err
}

func NewClient(stores ...kv.Store) Client {
	return &clientImpl{
		stores: stores,
	}
}
