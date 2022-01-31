package enterprise_testenv

import (
	"testing"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/backends/authdb"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/backends/redis_cache"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/backends/userdb"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/redisutil"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/healthcheck"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/stretchr/testify/assert"
)

type Options struct {
	RedisTarget string
}

func GetCustomTestEnv(t *testing.T, opts *Options) *testenv.TestEnv {
	env := testenv.GetTestEnv(t)
	if opts.RedisTarget != "" {
		healthChecker := healthcheck.NewTestingHealthChecker()
		redisClient := redisutil.NewClient(opts.RedisTarget, healthChecker, "cache_redis")
		env.SetCacheRedisClient(redisClient)
		env.SetRemoteExecutionRedisClient(redisClient)
		env.SetRemoteExecutionRedisPubSubClient(redisClient)
		log.Info("Using redis cache")
		cache := redis_cache.NewCache(redisClient, 500000000 /*=maxValueSizeBytes*/)
		env.SetCache(cache)
	}
	userDB, err := userdb.NewUserDB(env, env.GetDBHandle())
	if err != nil {
		assert.FailNow(t, "could not create user DB", err.Error())
	}
	env.SetUserDB(userDB)
	env.SetAuthDB(authdb.NewAuthDB(env.GetDBHandle()))
	return env
}