/*
Copyright (c) 2022 PaddlePaddle Authors. All Rights Reserve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"errors"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"github.com/PaddlePaddle/PaddleFlow/pkg/model"
)

// FsCacheStoreInterface currently has two implementations: DB and memory
// use newMemFSCache() or newDBFSCache(db *gorm.DB) to initiate
type FsCacheStoreInterface interface {
	Add(value *model.FSCache) error
	Get(fsID string, cacheID string) (*model.FSCache, error)
	Delete(fsID, cacheID string) error
	List(fsID, cacheID string) ([]model.FSCache, error)
	Update(value *model.FSCache) (int64, error)
}

func NewFsCacheStore(db *gorm.DB) FsCacheStoreInterface {
	// default use db storage, mem used in the future maybe as the cache for db
	return newDBFSCache(db)
}

func newMemFSCache() *MemFSCache {
	m := new(MemFSCache)
	m.fsCacheMap = newFSCacheMap()
	return m
}

func newDBFSCache(db *gorm.DB) FsCacheStoreInterface {
	n := new(DBFSCache)
	n.db = db
	return n
}

// ============================================= DB implementation ============================================= //

type DBFSCache struct {
	db *gorm.DB
}

func (f *DBFSCache) Add(value *model.FSCache) error {
	return f.db.Create(value).Error
}

func (f *DBFSCache) Get(fsID string, cacheID string) (*model.FSCache, error) {
	var fsCache model.FSCache
	tx := f.db.Where(&model.FSCache{FsID: fsID, CacheID: cacheID}).First(&fsCache)
	if tx.Error != nil {
		return nil, tx.Error
	}
	return &fsCache, nil
}

func (f *DBFSCache) Delete(fsID, cacheID string) error {
	result := f.db
	if fsID != "" {
		result.Where(fmt.Sprintf(QueryEqualWithParam, FsID), fsID)
	}
	if cacheID != "" {
		result.Where(fmt.Sprintf(QueryEqualWithParam, FsCacheID), cacheID)
	}
	// todo:// change to soft delete , update deleteAt = xx
	return result.Delete(&model.FSCache{}).Error
}

func (f *DBFSCache) List(fsID, cacheID string) ([]model.FSCache, error) {
	if fsID != "" {
		f.db.Where(fmt.Sprintf(QueryEqualWithParam, FsID), fsID)
	}
	if cacheID != "" {
		f.db.Where(fmt.Sprintf(QueryEqualWithParam, FsCacheID), cacheID)
	}
	var fsCaches []model.FSCache
	err := f.db.Find(&fsCaches).Error
	if err != nil {
		return nil, err
	}
	return fsCaches, nil
}

func (f *DBFSCache) Update(value *model.FSCache) (int64, error) {
	result := f.db.Where(&model.FSCache{FsID: value.FsID, CacheID: value.CacheID}).Updates(value)
	return result.RowsAffected, result.Error
}

// ============================================= memory implementation ============================================= //

type ConcurrentFSCacheMap struct {
	sync.RWMutex
	// key1:fsID key2:cacheID
	value map[string]map[string]*model.FSCache
}

func newFSCacheMap() *ConcurrentFSCacheMap {
	cm := new(ConcurrentFSCacheMap)
	cm.value = map[string]map[string]*model.FSCache{}
	return cm
}

func (cm *ConcurrentFSCacheMap) Get(key1, key2 string) *model.FSCache {
	cm.RLock()
	var retValue *model.FSCache
	if v1, ok := cm.value[key1]; ok {
		retValue = v1[key2]
	}
	cm.RUnlock()
	return retValue
}

func (cm *ConcurrentFSCacheMap) GetBatch(key string) []model.FSCache {
	cm.RLock()
	var tmp []model.FSCache
	if v, ok := cm.value[key]; ok {
		for _, v1 := range v {
			tmp = append(tmp, *v1)
		}
	}
	cm.RUnlock()
	return tmp
}

func (cm *ConcurrentFSCacheMap) Put(key string, value *model.FSCache) {
	cm.Lock()
	tempV := map[string]*model.FSCache{}
	if v, ok := cm.value[key]; ok {
		tempV = v
	}
	tempV[value.CacheID] = value
	cm.value[key] = tempV
	cm.Unlock()
}

func (cm *ConcurrentFSCacheMap) Delete(key1, key2 string) error {
	cm.Lock()
	var err error
	if cm.value != nil {
		if key1 != "" {
			if key2 != "" {
				fsMap := cm.value[key1]
				delete(fsMap, key2)
				cm.value[key1] = fsMap
			} else {
				delete(cm.value, key1)
			}
		}
	} else {
		err = errors.New("FSCache map is null")
	}
	cm.Unlock()
	return err
}

func (cm *ConcurrentFSCacheMap) Update(key, value *model.FSCache) (has bool, err error) {
	cm.Lock()
	defer cm.Unlock()
	if v1, ok := cm.value[value.FsID]; ok {
		_, ok = v1[value.CacheID]
		if ok {
			has = true
			v1[value.CacheID] = value
		}
	}
	return has, nil
}

type MemFSCache struct {
	fsCacheMap *ConcurrentFSCacheMap
}

func (mem *MemFSCache) Add(value *model.FSCache) error {
	mem.fsCacheMap.Put(value.FsID, value)
	return nil
}

func (mem *MemFSCache) Get(fsID string, cacheID string) (*model.FSCache, error) {
	return mem.fsCacheMap.Get(fsID, cacheID), nil
}

func (mem *MemFSCache) Delete(fsID, cacheID string) error {
	return mem.fsCacheMap.Delete(fsID, cacheID)
}

func (mem *MemFSCache) List(fsID, cacheID string) ([]model.FSCache, error) {
	var retMap []model.FSCache
	if fsID != "" {
		if cacheID != "" {
			retMap = append(retMap, *mem.fsCacheMap.Get(fsID, cacheID))
		} else {
			retMap = mem.fsCacheMap.GetBatch(fsID)
		}
	}
	return retMap, nil
}

func (mem *MemFSCache) Update(value *model.FSCache) (int64, error) {
	return 0, nil
}