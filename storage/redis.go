package storage

import (
	"encoding/json"
	"fmt"
	log "github.com/cihub/seelog"
	"github.com/garyburd/redigo/redis"
	"sort"
	"strings"
	"time"
)

type RedisStorage struct {
	pool  *redis.Pool
	retry int
}

func NewRedisStorage(server string, pass string, poolsize int, retry int) *RedisStorage {
	return &RedisStorage{
		pool: &redis.Pool{
			MaxActive:   poolsize,
			MaxIdle:     poolsize,
			IdleTimeout: 300 * time.Second,
			Dial: func() (redis.Conn, error) {
				c, err := redis.Dial("tcp", server)
				if err != nil {
					log.Infof("failed to connect Redis (%s), (%s)", server, err)
					return nil, err
				}
				if _, err := c.Do("AUTH", pass); err != nil {
					log.Infof("failed to auth Redis (%s), (%s)", server, err)
					return nil, err

				}
				log.Infof("connected with Redis (%s)", server)
				return c, err
			},
			TestOnBorrow: func(c redis.Conn, t time.Time) error {
				_, err := c.Do("PING")
				return err
			},
		},
		retry: retry,
	}
}

func (r *RedisStorage) Do(commandName string, args ...interface{}) (interface{}, error) {
	var conn redis.Conn
	i := r.retry
	for ; i > 0; i-- {
		conn = r.pool.Get()
		err := conn.Err()
		if err == nil {
			break
		} else {
			log.Infof("failed to get conn from pool (%s)", err)
		}
		time.Sleep(time.Second)
	}
	if i == 0 || conn == nil {
		return nil, fmt.Errorf("failed to find a useful redis conn")
	} else {
		ret, err := conn.Do(commandName, args...)
		conn.Close()
		return ret, err
	}
}

// 从存储后端获取 > 指定时间的所有消息
func (r *RedisStorage) GetOfflineMsgs(appId string, regId string, msgId int64) []*RawMessage {
	key := "db_offline_msg_" + appId
	ret, err := redis.Strings(r.Do("HKEYS", key))
	if err != nil {
		log.Infof("failed to get fields of offline msg:", err)
		return nil
	}

	now := time.Now().Unix()
	skeys := make(map[int64]interface{})
	var sidxs []float64

	for i := range ret {
		var (
			idx    int64
			expire int64
		)
		if _, err := fmt.Sscanf(ret[i], "%v_%v", &idx, &expire); err != nil {
			log.Infof("invaild redis hash field:", err)
			continue
		}

		if idx <= msgId || expire <= now {
			continue
		} else {
			skeys[idx] = ret[i]
			sidxs = append(sidxs, float64(idx))
		}
	}

	sort.Float64Slice(sidxs).Sort()
	args := []interface{}{key}
	for k := range sidxs {
		t := int64(sidxs[k])
		args = append(args, skeys[t])
	}

	if len(args) == 1 {
		return nil
	}

	rmsgs, err := redis.Strings(r.Do("HMGET", args...))
	if err != nil {
		log.Infof("failed to get offline rmsg:", err)
		return nil
	}

	var msgs []*RawMessage
	for i := range rmsgs {
		t := []byte(rmsgs[i])
		msg := &RawMessage{}
		if err := json.Unmarshal(t, msg); err != nil {
			log.Infof("failed to decode raw msg:", err)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// 从存储后端获取指定消息
func (r *RedisStorage) GetRawMsg(appId string, msgId int64) *RawMessage {
	key := "db_msg_" + appId
	ret, err := redis.Bytes(r.Do("HGET", key, msgId))
	if err != nil {
		log.Warnf("redis: HGET failed (%s)", err)
		return nil
	}
	rmsg := &RawMessage{}
	if err := json.Unmarshal(ret, rmsg); err != nil {
		log.Warnf("failed to decode raw msg:", err)
		return nil
	}
	return rmsg
}

func (r *RedisStorage) AddDevice(serverName, devId string) error {
	_, err := redis.Int(r.Do("HSET", "db_comet_"+serverName, devId, nil))
	if err != nil {
		log.Warnf("redis: HSET failed (%s)", err)
		return err
	}
	return nil
}

func (r *RedisStorage) RemoveDevice(serverName, devId string) error {
	_, err := r.Do("HDEL", "db_comet_"+serverName, devId)
	if err != nil {
		log.Warnf("redis: HDEL failed (%s)", err)
		return err
	}
	return nil
}

func (r *RedisStorage) CheckDevice(devId string) (string, error) {
	keys, err := redis.Strings(r.Do("KEYS", "db_comet_*"))
	if err != nil {
		log.Errorf("failed to get comet nodes KEYS:", err)
		return "", err
	}
	for _, key := range keys {
		exist, err := r.HashExists(key, devId)
		if err != nil {
			log.Errorf("error on HashExists:", err)
			return "", err
		}
		if exist == 1 {
			return strings.TrimPrefix(key, "db_comet_"), nil
		}
	}
	return "", nil
}

func (r *RedisStorage) RefreshDevices(serverName string, timeout int) error {
	_, err := redis.Int(r.Do("EXPIRE", "db_comet_"+serverName, timeout))
	if err != nil {
		log.Warnf("redis: EXPIRE failed, (%s)", err)
	}
	return err
}

func (r *RedisStorage) InitDevices(serverName string) error {
	_, err := redis.Int(r.Do("DEL", "db_comet_"+serverName))
	if err != nil {
		log.Warnf("redis: DEL failed, (%s)", err)
	}
	return err
}

func (r *RedisStorage) HashGetAll(db string) ([]string, error) {
	ret, err := r.Do("HGETALL", db)
	if err != nil {
		log.Warnf("redis: HGET failed (%s)", err)
		return nil, err
	}
	if ret != nil {
		ret, err := redis.Strings(ret, nil)
		if err != nil {
			log.Warnf("redis: convert to strings failed (%s)", err)
		}
		return ret, err
	}
	return nil, nil
}

func (r *RedisStorage) HashGet(db string, key string) ([]byte, error) {
	ret, err := r.Do("HGET", db, key)
	if err != nil {
		log.Warnf("redis: HGET failed (%s)", err)
		return nil, err
	}
	if ret != nil {
		ret, err := redis.Bytes(ret, nil)
		if err != nil {
			log.Warnf("redis: convert to bytes failed (%s)", err)
		}
		return ret, err
	}
	return nil, nil
}

func (r *RedisStorage) HashSet(db string, key string, val []byte) (int, error) {
	ret, err := redis.Int(r.Do("HSET", db, key, val))
	if err != nil {
		log.Warnf("redis: HSET failed (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) HashExists(db string, key string) (int, error) {
	ret, err := redis.Int(r.Do("HEXISTS", db, key))
	if err != nil {
		log.Warnf("redis: HEXISTS failed (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) HashSetNotExist(db string, key string, val []byte) (int, error) {
	ret, err := redis.Int(r.Do("HSETNX", db, key, val))
	if err != nil {
		log.Warnf("redis: HSETNX failed (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) HashDel(db string, key string) (int, error) {
	ret, err := redis.Int(r.Do("HDEL", db, key))
	if err != nil {
		log.Warnf("redis: HDEL failed (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) HashIncrBy(db string, key string, val int64) (int64, error) {
	ret, err := redis.Int64(r.Do("HINCRBY", db, key, val))
	if err != nil {
		log.Warnf("redis: HINCRBY failed, (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) SetNotExist(key string, val []byte) (int, error) {
	ret, err := redis.Int(r.Do("SETNX", key, val))
	if err != nil {
		log.Warnf("redis: SETNX failed (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) IncrBy(key string, val int64) (int64, error) {
	ret, err := redis.Int64(r.Do("INCRBY", key, val))
	if err != nil {
		log.Warnf("redis: INCRBY failed, (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) SetAdd(key string, val string) (int, error) {
	ret, err := redis.Int(r.Do("SADD", key, val))
	if err != nil {
		log.Warnf("redis: SADD failed, (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) SetDel(key string, val string) (int, error) {
	ret, err := redis.Int(r.Do("SREM", key, val))
	if err != nil {
		log.Warnf("redis: SREM failed, (%s)", err)
	}
	return ret, err
}

func (r *RedisStorage) SetIsMember(key string, val string) (int, error) {
	ret, err := redis.Int(r.Do("SISMEMBER", key, val))
	if err != nil {
		log.Warnf("redis: SISMEMBER failed, (%s)", err)
	}
	return ret, err

}

func (r *RedisStorage) SetMembers(key string) ([]string, error) {
	ret, err := redis.Strings(r.Do("SMEMBERS", key))
	if err != nil {
		log.Warnf("redis: SMEMBERS failed, (%s)", err)
	}
	return ret, err
}
