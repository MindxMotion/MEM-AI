#!/usr/bin/env bash
# Explicit, reviewed provisioning only. Never starts/restarts a workload.
set +x
set -euo pipefail
umask 077
[[ "${1:-}" == "--apply" && "$#" == 1 ]] || {
  printf 'Requires reviewed --apply; creates only New API Redis credentials\n' >&2
  exit 1
}
context="${DEPLOY_CONTEXT:?DEPLOY_CONTEXT is required}"
namespace=new-api
target=/root/new-api-deploy/redis.env
[[ "$(id -u)" == 0 && "$(kubectl config current-context)" == "${context}" ]] || exit 1
[[ -d /root/new-api-deploy && ! -L /root/new-api-deploy \
  && "$(stat -c '%u:%a' /root/new-api-deploy)" == "0:700" \
  && ! -e "${target}" && ! -L "${target}" ]] || {
  printf 'REDIS_CREDENTIAL_PACKAGE_EXISTS_OR_DIRECTORY_INVALID\n' >&2
  exit 1
}
# Namespace is parent-owned; missing namespace stops before generating credentials.
kubectl --context "${context}" get namespace "${namespace}" --output=name >/dev/null
existing="$(kubectl --context "${context}" --namespace "${namespace}" get secret \
  new-api-redis-auth --ignore-not-found --output=name)"
[[ -z "${existing}" ]] || {
  printf 'NEW_API_REDIS_SECRET_EXISTS: reconcile privately, do not rotate\n' >&2
  exit 1
}
temp_dir="$(mktemp -d /dev/shm/new-api-redis.XXXXXXXX)"
pending=""
trap 'rm -rf "${temp_dir}"; if [[ -n "${pending}" ]]; then rm -f "${pending}"; fi' EXIT
openssl rand -hex 32 | tr -d '\n' >"${temp_dir}/password"
password="$(cat "${temp_dir}/password")"
[[ "${password}" =~ ^[a-f0-9]{64}$ ]] || exit 1
printf 'REDIS_USERNAME=new_api\nREDIS_PASSWORD=%s\nREDIS_CONN_STRING=redis://new_api:%s@new-api-redis.new-api.svc.cluster.local:6379/0\n' \
  "${password}" "${password}" >"${temp_dir}/redis.env"
unset password
pending="$(mktemp /root/new-api-deploy/.redis.env.XXXXXXXX)"
install -m 600 "${temp_dir}/redis.env" "${pending}"
# Atomic publication refuses overwriting a concurrent credential package.
ln "${pending}" "${target}"
# create, not apply: refuses overwriting an existing Secret; no Secret export.
if ! kubectl --context "${context}" --namespace "${namespace}" create secret generic \
  new-api-redis-auth --from-file=password="${temp_dir}/password" >/dev/null 2>&1; then
  # A lost API response does not prove creation failed. Preserve the private
  # package so the owner can reconcile equality without rotating or losing it.
  printf 'NEW_API_REDIS_SECRET_CREATE_UNCONFIRMED: private package retained; reconcile before retry\n' >&2
  exit 1
fi
printf 'NEW_API_REDIS_CREDENTIALS_CREATED user=new_api db=0 mode=0600; workloads unchanged\n'
