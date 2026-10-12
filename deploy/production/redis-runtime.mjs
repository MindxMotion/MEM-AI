import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

export async function redisManifest() {
  const source = (await readFile(new URL("redis.yaml", import.meta.url), "utf8")).replaceAll("\r\n", "\n");
  const image = source.match(/^          image: (.+)$/m)?.[1];
  const block = source.match(/            - \|\n([\s\S]*?)          ports:/)?.[1];
  assert.ok(image && block, "Redis image and startup must come from the actual manifest");
  return { image, startup: block.split("\n").map(line => line.replace(/^              /, "")).join("\n") };
}

export async function runtimeContract() {
  const { image, startup } = await redisManifest();
  const fixture = "/run/redis/fixtures/password";
  const command = startup.replaceAll("/run/redis-auth/password", fixture)
    .replace("'bind 0.0.0.0'", "'bind 127.0.0.1'")
    .replace("exec redis-server /run/redis/redis.conf", "redis-server /run/redis/redis.conf >/dev/null 2>&1 &\nredis_pid=$!");
  const checks = (await readFile(new URL("redis-acl-test.sh", import.meta.url), "utf8"))
    .replaceAll("\r\n", "\n").replaceAll("/fixtures/password", fixture);
  const script = `set +x\nset -eu\numask 077\nmkdir -p /run/redis/fixtures\n`
    + `head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n' > ${fixture}\n`
    + `${command}\n${checks}\nkill "$redis_pid"; wait "$redis_pid"\n${command}\n${checks}`;
  return { image, script };
}

export async function testResources(name) {
  assert.match(name, /^new-api-redis-acl-[a-z0-9]{8,32}$/);
  const { image, script } = await runtimeContract();
  const labels = { "new-api.dev/redis-acl-test": name };
  // Keep this owned Pod alive until the driver verifies its kernel deny-all
  // policy. The loop is bounded; fixtures and Redis are not started beforehand.
  const gatedScript = "set +x\nset -eu\nattempts=0\n"
    + "until test -f /run/redis/isolation-ready; do attempts=$((attempts + 1)); test \"$attempts\" -lt 300; sleep 0.1; done\n"
    + script;
  return { apiVersion: "v1", kind: "List", items: [
    { apiVersion: "networking.k8s.io/v1", kind: "NetworkPolicy", metadata: { name, namespace: "new-api" },
      spec: { podSelector: { matchLabels: labels }, policyTypes: ["Ingress", "Egress"], ingress: [], egress: [] } },
    { apiVersion: "batch/v1", kind: "Job", metadata: { name, namespace: "new-api" },
      spec: { backoffLimit: 0, activeDeadlineSeconds: 90, ttlSecondsAfterFinished: 300,
        template: { metadata: { labels }, spec: {
          restartPolicy: "Never", automountServiceAccountToken: false, enableServiceLinks: false, hostNetwork: false,
          nodeSelector: { "kubernetes.io/hostname": "node1" },
          securityContext: { runAsNonRoot: true, runAsUser: 999, runAsGroup: 999, fsGroup: 999,
            seccompProfile: { type: "RuntimeDefault" } },
          containers: [{ name: "redis-acl-test", image, imagePullPolicy: "Never", command: ["/bin/sh", "-ec", gatedScript],
            securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } },
            resources: { requests: { cpu: "50m", memory: "64Mi" }, limits: { cpu: "250m", memory: "192Mi" } },
            volumeMounts: [{ name: "runtime", mountPath: "/run/redis" }] }],
          volumes: [{ name: "runtime", emptyDir: { medium: "Memory", sizeLimit: "1Mi" } }]
        } } } }
  ] };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.stdout.write(`${JSON.stringify(await testResources(process.argv[2]))}\n`);
}
