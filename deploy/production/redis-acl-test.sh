#!/bin/sh
# OFF-PRODUCTION only: no network, no real connection strings or user records.
set +x
set -eu
export REDISCLI_AUTH="$(cat /fixtures/password)"
cli() { redis-cli --user new_api --raw "$@" 2>/dev/null; }
stage=connection
fail() { printf 'NEW_API_REDIS_ACL_TEST_FAILED stage=%s\n' "$stage" >&2; exit 1; }
allow() { result="$(cli "$@")" || fail; case "$result" in *ERR*|*NOPERM*|*NOAUTH*) fail ;; esac; }
deny() {
  stage="deny-$1"
  result="$(cli "$@")" || :
  case "$result" in
    *NOPERM*|*"has no permissions"*|*"can't run this command"*) ;;
    *"DEBUG command not allowed"*) test "$1" = debug || fail ;;
    *) fail ;;
  esac
}
subscriber_pid=""
trap 'if test -n "$subscriber_pid"; then kill "$subscriber_pid" 2>/dev/null || :; fi' EXIT
wait_subscription() {
  attempts=0
  while :; do
    if test "$1" = ack; then
      if grep -qx subscribe /run/redis/subscription && grep -qx 1 /run/redis/subscription; then return; fi
    elif grep -qx message /run/redis/subscription && grep -qx synthetic /run/redis/subscription; then
      return
    fi
    kill -0 "$subscriber_pid" 2>/dev/null || fail
    attempts=$((attempts + 1)); test "$attempts" -lt 50 || fail
    sleep 0.1
  done
}
i=0
until test "$(cli ping)" = PONG; do
  i=$((i + 1)); test "$i" -lt 30 || fail; sleep 0.1
done
# These are synthetic New API cache keys, not token material or user data.
stage=cache-keys
for key in user:900001 token:synthetic auth:user:fence:900001 auth:user:version:900001 \
  auth:session:synthetic rateLimit:v2:ip:CT:synthetic rateLimit:900001 rateLimit:MRRLS:900001 \
  notify_limit:900001:test:synthetic perf:synthetic:synthetic:0 \
  new-api:subscription_plan:v1:synthetic new-api:subscription_plan_info:v1:synthetic \
  new-api:channel_affinity:v1:synthetic new-api:channel_affinity_usage_cache_stats:v1:synthetic; do
  allow set "$key" synthetic EX 60
  test "$(cli get "$key")" = synthetic || fail
  allow unlink "$key"
done
# Prove cachex scan and bulk invalidation parity inside this owned instance.
stage=scan
allow set new-api:channel_affinity:v1:synthetic synthetic EX 60
result="$(cli scan 0 MATCH 'new-api:channel_affinity:v1:*' COUNT 1000)"
case "$result" in *new-api:channel_affinity:v1:synthetic*) ;; *) fail ;; esac
test "$(cli unlink new-api:channel_affinity:v1:synthetic)" = 1 || fail
stage=lua
allow eval "local n=redis.call('INCR',KEYS[1]);redis.call('EXPIRE',KEYS[1],60);return {n,redis.call('TTL',KEYS[1])}" 1 rateLimit:v2:synthetic
allow eval "redis.call('HSET',KEYS[1],'Id',900001,'Quota',10);return redis.call('HINCRBY',KEYS[1],'Quota',-1)" 1 user:900001
sha="$(cli script load "redis.call('TIME');redis.call('HMSET',KEYS[1],'tokens',10);return redis.call('HMGET',KEYS[1],'tokens')")"
allow evalsha "$sha" 1 rateLimit:900001
allow eval "redis.call('HSET',KEYS[1],'Status','active');redis.call('PEXPIRE',KEYS[1],60000);redis.call('PEXPIREAT',KEYS[1],4102444800000);return redis.call('HGET',KEYS[1],'Status')" 1 auth:session:synthetic
stage=transactions
result="$(printf 'MULTI\nHINCRBY perf:synthetic:synthetic:0 req 1\nEXPIRE perf:synthetic:synthetic:0 60\nEXEC\n' | cli)"
case "$result" in *NOPERM*|*ERR*) fail ;; esac
allow lpush rateLimit:MRRLS:900001 synthetic
allow llen rateLimit:MRRLS:900001
allow lindex rateLimit:MRRLS:900001 -1
allow ltrim rateLimit:MRRLS:900001 0 10
stage=pubsub
redis-cli --user new_api --raw subscribe new-api:wsmanager:channel-close:db:0 \
  >/run/redis/subscription 2>/dev/null &
subscriber_pid=$!
wait_subscription ack
test "$(cli publish new-api:wsmanager:channel-close:db:0 synthetic)" = 1 || fail
wait_subscription message
kill "$subscriber_pid"
wait "$subscriber_pid" || :
subscriber_pid=""
grep -q synthetic /run/redis/subscription || fail
deny publish new-api:wsmanager:channel-close:db:1 synthetic
deny subscribe unrelated:synthetic
# Exact allowlist rejects administration, persistence and connection termination.
stage=admin-denials
deny flushdb
deny flushall
deny config get maxmemory
deny acl users
deny shutdown nosave
deny replicaof no one
deny migrate 127.0.0.1 1 synthetic 0 1
deny module list
deny debug help
deny save
deny bgsave
deny client kill type normal
deny script flush
stage=lua-admin-denial
deny eval "return redis.call('FLUSHDB')" 0
# All keys are owned by this instance; SELECT1 must fail because only DB0 exists.
stage=database-boundary
result="$(cli select 1)"
case "$result" in *"DB index is out of range"*) ;; *) fail ;; esac
stage=default-denial
result="$(redis-cli --user default --raw ping 2>&1)" || :
case "$result" in *WRONGPASS*|*NOAUTH*) ;; *) fail ;; esac
unset REDISCLI_AUTH
result="$(redis-cli --raw ping 2>&1)" || :
case "$result" in *NOAUTH*) ;; *) fail ;; esac
printf 'NEW_API_REDIS_ACL_TEST_PASSED\n'
