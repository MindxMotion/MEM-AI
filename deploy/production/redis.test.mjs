import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { redisManifest, runtimeContract, testResources } from "./redis-runtime.mjs";

const directory = new URL("./", import.meta.url);
const image = "redis:7.4.11-alpine3.21@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499";

test("dedicated Redis manifest keeps traffic, resource and credential ownership local", async () => {
  const source = (await readFile(new URL("redis.yaml", directory), "utf8")).replaceAll("\r\n", "\n");
  assert.equal((source.match(/^  namespace: new-api$/gm) ?? []).length, 3);
  assert.match(source, /namespaceSelector:[\s\S]*kubernetes.io\/metadata.name: new-api[\s\S]*          podSelector:[\s\S]*app.kubernetes.io\/name: new-api\n/);
  assert.match(source, /policyTypes: \[Ingress\]/);
  assert.match(source, /replicas: 1/);
  assert.match(source, /type: Recreate/);
  assert.match(source, /type: ClusterIP/);
  assert.match(source, /memory: 192Mi/);
  assert.match(source, /automountServiceAccountToken: false/);
  assert.match(source, /runAsUser: 999/);
  assert.match(source, /type: RuntimeDefault/);
  assert.match(source, /drop: \[ALL\]/);
  assert.match(source, /medium: Memory/);
  assert.match(source, /secretName: new-api-redis-auth/);
  assert.ok(source.includes(image));
  assert.doesNotMatch(source, /kind: Namespace|persistentVolumeClaim|hostPath|hostPort|NodePort|LoadBalancer|games-redis-auth/);
});

test("restart startup disables default and writes only named password hash to tmpfs", async () => {
  const command = (await redisManifest()).startup;
  const permissions = command.match(/'(\+auth [^'\n]+)'/)?.[1];
  assert.ok(permissions);
  assert.doesNotMatch(permissions, /\+@|\+(?:acl|config|flushdb|flushall|shutdown|replicaof|migrate|module|debug|save|bgsave|client\|kill)\b/);
  for (const required of ["scan", "eval", "evalsha", "script|load", "time", "hmget", "hmset", "pexpireat", "multi", "exec", "unlink"]) {
    assert.ok(permissions.split(" ").includes(`+${required}`), required);
  }
  assert.match(command, /&new-api:wsmanager:channel-close:db:0 -@all/);
  assert.match(command, /databases 1/);
  assert.match(command, /maxmemory 128mb/);
  assert.match(command, /maxmemory-policy noeviction/);
  assert.match(command, /save ""/);
  assert.match(command, /appendonly no/);
  const fixture = await mkdtemp(join(tmpdir(), "new-api-redis-startup-"));
  try {
    const password = randomBytes(32).toString("hex");
    await writeFile(join(fixture, "password"), password);
    const script = command.replaceAll("/run/redis-auth/password", `${fixture.replaceAll("\\", "/")}/password`)
      .replaceAll("/run/redis", fixture.replaceAll("\\", "/")).replace(/exec redis-server .*/, ":");
    for (let restart = 0; restart < 2; restart++) {
      const result = spawnSync("bash", ["-ec", script], { encoding: "utf8", timeout: 10_000 });
      assert.ifError(result.error);
      assert.equal(result.status, 0);
      assert.equal(result.stdout + result.stderr, "");
      const acl = await readFile(join(fixture, "users.acl"), "utf8");
      assert.ok(acl.startsWith("user default reset off\nuser new_api reset on #"));
      assert.ok(acl.includes(createHash("sha256").update(password).digest("hex")));
      assert.ok(!acl.includes(password));
      assert.equal(acl.trim().split("\n").length, 2);
    }
  } finally { await rm(fixture, { recursive: true, force: true }); }
});

test("provisioning requires explicit apply and leaves other credential packages alone", async () => {
  const source = await readFile(new URL("redis-secret.sh", directory), "utf8");
  assert.match(source, /set \+x/);
  assert.match(source, /install -m 600/);
  assert.match(source, /ln "\$\{pending\}" "\$\{target\}"/);
  assert.match(source, /new-api-redis\.new-api\.svc\.cluster\.local:6379\/0/);
  assert.match(source, /get namespace "\$\{namespace\}"/);
  assert.doesNotMatch(source, /credentials\.env|mindmotion\/|games-redis|--from-literal|output=yaml|rollout|apply -f/);
  const result = spawnSync("bash", ["-s"], { input: source, encoding: "utf8", timeout: 10_000 });
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Requires reviewed --apply/);
});

test("private provisioning refuses drift and retains credentials on unconfirmed API creation", async () => {
  const source = (await readFile(new URL("redis-secret.sh", directory), "utf8")).replaceAll("\r\n", "\n");
  for (const state of ["ready", "namespace-missing", "identity-exists", "file-exists", "create-failed", "wrong-context"]) {
    const fixture = await mkdtemp(join(tmpdir(), "new-api-redis-secret-"));
    try {
      const packagePath = join(fixture, "package");
      await mkdir(packagePath, { mode: 0o700 });
      if (state === "file-exists") await writeFile(join(packagePath, "redis.env"), "untouched-fixture");
      const script = source.replaceAll("/root/new-api-deploy", packagePath.replaceAll("\\", "/"))
        .replaceAll("/dev/shm", fixture.replaceAll("\\", "/"));
      const result = spawnSync("bash", ["-s", "--", "--apply"], {
        env: { ...process.env, DEPLOY_CONTEXT: "example-context" },
        encoding: "utf8", timeout: 10_000,
        input: `id() { printf 0; }
stat() { printf '0:700'; }
openssl() { printf '%064d\\n' 0; }
kubectl() {
  if [[ "$*" == "config current-context" ]]; then
    printf '${state === "wrong-context" ? "wrong" : "example-context"}'
  elif [[ "$*" == *"get namespace"* ]]; then
    ${state === "namespace-missing" ? "return 1" : ":"}
  elif [[ "$*" == *"get secret"* ]]; then
    ${state === "identity-exists" ? "printf new-api-redis-auth" : ":"}
  elif [[ "$*" == *"create secret"* ]]; then
    ${state === "create-failed" ? "return 1" : ":"}
  else return 99; fi
}
${script}` });
      assert.ifError(result.error);
      assert.equal(result.status, state === "ready" ? 0 : 1, `provisioning state ${state}`);
      assert.ok(!result.stdout.includes("0".repeat(64)) && !result.stderr.includes("0".repeat(64)));
      const members = await readdir(packagePath);
      if (state === "ready" || state === "create-failed") {
        assert.deepEqual(members, ["redis.env"]);
        const env = await readFile(join(packagePath, "redis.env"), "utf8");
        assert.ok(env.includes("@new-api-redis.new-api.svc.cluster.local:6379/0"));
        if (state === "create-failed") assert.match(result.stderr, /reconcile before retry/);
      } else if (state === "file-exists") {
        assert.equal(await readFile(join(packagePath, "redis.env"), "utf8"), "untouched-fixture");
      } else assert.deepEqual(members, []);
      assert.deepEqual(await readdir(fixture), ["package"], "temporary packages removed");
    } finally { await rm(fixture, { recursive: true, force: true }); }
  }
});

test("synthetic Job uses the actual startup/image with deny-all and bounded unprivileged lifecycle", async () => {
  const name = "new-api-redis-acl-localtest";
  const resources = await testResources(name);
  const [policy, job] = resources.items;
  assert.equal(policy.metadata.namespace, "new-api");
  assert.equal(job.metadata.namespace, "new-api");
  assert.deepEqual(policy.spec.podSelector.matchLabels, job.spec.template.metadata.labels);
  assert.deepEqual(policy.spec.policyTypes, ["Ingress", "Egress"]);
  assert.deepEqual(policy.spec.ingress, []);
  assert.deepEqual(policy.spec.egress, []);
  assert.equal(job.spec.backoffLimit, 0);
  assert.equal(job.spec.activeDeadlineSeconds, 90);
  assert.equal(job.spec.ttlSecondsAfterFinished, 300);
  const pod = job.spec.template.spec;
  assert.equal(pod.automountServiceAccountToken, false);
  assert.equal(pod.hostNetwork, false);
  assert.equal(pod.securityContext.runAsUser, 999);
  assert.equal(pod.securityContext.seccompProfile.type, "RuntimeDefault");
  const container = pod.containers[0];
  assert.equal(container.image, image);
  assert.equal(container.imagePullPolicy, "Never");
  assert.equal(container.securityContext.readOnlyRootFilesystem, true);
  assert.deepEqual(container.securityContext.capabilities.drop, ["ALL"]);
  assert.equal(container.resources.limits.memory, "192Mi");
  assert.equal(container.resources.limits.cpu, "250m");
  assert.equal(pod.volumes[0].emptyDir.medium, "Memory");
  assert.match(container.command[2], /head -c 32 \/dev\/urandom/);
  assert.match(container.command[2], /test "\$attempts" -lt 300/);
  assert.ok(container.command[2].indexOf("isolation-ready") < container.command[2].indexOf("/dev/urandom"));
  assert.match(container.command[2], /bind 127\.0\.0\.1/);
  assert.doesNotMatch(JSON.stringify(resources), /secretName|serviceAccountName|hostPath|\/root\/new-api-deploy|redis:\/\/|games-redis/);
  assert.ok(container.command[2].includes((await redisManifest()).startup.match(/'\+auth [^'\n]+'/)[0]));
  const checks = await readFile(new URL("redis-acl-test.sh", directory), "utf8");
  assert.doesNotMatch(checks, /sleep 0\.2/);
  assert.match(checks, /wait_subscription ack/);
  assert.match(checks, /wait_subscription message/);
});

test("pinned Redis runtime: SCAN parity, scripts, transactions, channels, restart and admin/default denials",
  { skip: process.env.NEW_API_REDIS_DOCKER_TEST !== "1" }, async () => {
    const containerName = `new-api-redis-acl-${randomBytes(6).toString("hex")}`;
    try {
      const { image: runtimeImage, script } = await runtimeContract();
      const result = spawnSync("docker", ["run", "--rm", "--name", containerName, "--pull=never",
        "--network", "none", "--read-only", "--user", "999:999",
        "--tmpfs", "/run/redis:rw,uid=999,gid=999,mode=0700", runtimeImage, "/bin/sh", "-ec", script],
      { encoding: "utf8", timeout: 30_000 });
      assert.ifError(result.error);
      assert.equal(result.status, 0, "isolated Redis runtime failed; raw output withheld");
      assert.equal(result.stdout.trim(), "NEW_API_REDIS_ACL_TEST_PASSED\nNEW_API_REDIS_ACL_TEST_PASSED");
    } finally {
      // Remove only this test's uniquely named container, including on timeout.
      spawnSync("docker", ["rm", "--force", containerName], { stdio: "ignore", timeout: 10_000 });
    }
  });
