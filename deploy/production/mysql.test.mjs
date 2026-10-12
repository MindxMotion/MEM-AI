import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";

const directory = new URL("./", import.meta.url);
const [pvc, policy, deployment, service] = JSON.parse(readFileSync(new URL("mysql.json", directory), "utf8")).items;
const pod = deployment.spec.template.spec;
const mysql = pod.containers[0];

test("dedicated MySQL pins proven runtime, restricted filesystem, TLS and same-namespace-only app ingress", () => {
  for (const item of [pvc, policy, deployment, service]) {
    assert.equal(item.metadata.namespace, "new-api");
    assert.equal(item.metadata.labels["app.kubernetes.io/part-of"], "new-api");
  }
  assert.deepEqual(pvc.spec, { storageClassName: "local-path", accessModes: ["ReadWriteOnce"], resources: { requests: { storage: "1Gi" } } });
  assert.equal(deployment.spec.replicas, 1);
  assert.deepEqual(deployment.spec.strategy, { type: "Recreate" });
  assert.deepEqual(deployment.spec.selector.matchLabels, { "app.kubernetes.io/name": "new-api-mysql" });
  assert.deepEqual(pod.nodeSelector, { "kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux" });
  assert.equal(pod.automountServiceAccountToken, false);
  assert.equal(pod.hostNetwork, false);
  assert.deepEqual(pod.securityContext, {
    runAsNonRoot: true, runAsUser: 999, runAsGroup: 999, fsGroup: 999,
    fsGroupChangePolicy: "OnRootMismatch", seccompProfile: { type: "RuntimeDefault" },
  });
  assert.deepEqual(mysql.securityContext, { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] } });
  assert.equal(mysql.image, "mysql:8.4.8@sha256:4fc4bca5a26c5aa2246a92779d238bc2d34f0373e65dc083d7b635b02780dd3d");
  assert.equal(mysql.command, undefined, "retain actual official image entrypoint");
  assert.equal(mysql.args[0], "mysqld");
  for (const argument of [
    "--skip-log-bin", "--mysqlx=OFF", "--performance-schema=OFF", "--max-connections=30",
    "--innodb-buffer-pool-size=128M", "--innodb-redo-log-capacity=64M",
    "--secure-file-priv=NULL", "--require-secure-transport=ON",
    "--ssl-ca=/run/mysql-auth/ca.pem", "--ssl-cert=/run/mysql-auth/server.pem",
    "--ssl-key=/run/mysql-auth/server-key.pem",
  ]) assert.ok(mysql.args.includes(argument), argument);
  assert.deepEqual(mysql.resources, { requests: { cpu: "250m", memory: "512Mi" }, limits: { cpu: "1000m", memory: "1536Mi" } });
  assert.deepEqual(Object.fromEntries(mysql.env.map(entry => [entry.name, entry.value])), {
    MYSQL_DATABASE: "new_api", MYSQL_USER: "new_api_app", MYSQL_ROOT_HOST: "localhost",
    MYSQL_ROOT_PASSWORD_FILE: "/run/mysql-auth/root-password",
    MYSQL_PASSWORD_FILE: "/run/mysql-auth/app-password", MYSQL_INITDB_SKIP_TZINFO: "1",
  });
  assert.deepEqual(pod.volumes, [
    { name: "data", persistentVolumeClaim: { claimName: "new-api-mysql-data" } },
    { name: "temporary", emptyDir: { sizeLimit: "64Mi" } },
    { name: "socket", emptyDir: { sizeLimit: "8Mi" } },
    { name: "auth", secret: { secretName: "new-api-mysql-auth", defaultMode: 0o440 } },
  ]);
  assert.deepEqual(mysql.volumeMounts, [
    { name: "data", mountPath: "/var/lib/mysql" }, { name: "temporary", mountPath: "/tmp" },
    { name: "socket", mountPath: "/var/run/mysqld" },
    { name: "auth", mountPath: "/run/mysql-auth", readOnly: true },
  ]);
  for (const probe of ["startupProbe", "readinessProbe"]) assert.deepEqual(mysql[probe].tcpSocket, { port: "mysql" });
  assert.match(mysql.livenessProbe.exec.command[2], /^mysqladmin --protocol=socket .* ping --silent >\/dev\/null 2>&1$/);
  assert.equal(service.spec.type, "ClusterIP");
  assert.deepEqual(service.spec.selector, deployment.spec.selector.matchLabels);
  assert.deepEqual(service.spec.ports, [{ name: "mysql", port: 3306, targetPort: "mysql", protocol: "TCP" }]);
  assert.deepEqual(policy.spec, {
    podSelector: { matchLabels: { "app.kubernetes.io/name": "new-api-mysql" } }, policyTypes: ["Ingress"],
    ingress: [{ from: [{
      namespaceSelector: { matchLabels: { "kubernetes.io/metadata.name": "new-api" } },
      podSelector: { matchLabels: { "app.kubernetes.io/name": "new-api" } },
    }], ports: [{ protocol: "TCP", port: 3306 }] }],
  });
  assert.doesNotMatch(JSON.stringify([pod, policy, service]), /hostPort|hostPath|NodePort|LoadBalancer|apisix|GRANT OPTION|MYSQL_RANDOM_ROOT_PASSWORD/);
  const appPod = JSON.parse(readFileSync(new URL("application.json", directory), "utf8")).items[2].spec.template.spec;
  assert.ok(!JSON.stringify(appPod).includes("new-api-mysql-auth"));
  const appEnv = Object.fromEntries(appPod.containers[0].env.map(entry => [entry.name, entry.value]));
  assert.equal(appEnv.SQL_MAX_OPEN_CONNS, "10");
  assert.equal(appEnv.SQL_MAX_IDLE_CONNS, "2");
  assert.equal(appEnv.LOG_SQL_DSN, "");
  assert.ok(Number(appEnv.SQL_MAX_OPEN_CONNS) < 30);
});

test("real OpenSSL synthetic provisioning: literal config, certificate identity, drift and response-loss preservation", () => {
  const openssl = process.platform === "win32" ? "C:/Program Files/Git/usr/bin/openssl.exe" : "openssl";
  const availability = spawnSync(openssl, ["version"], { encoding: "utf8", timeout: 10000 });
  assert.ifError(availability.error);
  assert.equal(availability.status, 0, "OpenSSL unavailable: TLS fixture cannot claim passing");
  const script = String.raw`
import importlib.util, io, json, os, pathlib, stat, subprocess, sys, tempfile, unittest
from contextlib import redirect_stdout, redirect_stderr
from types import SimpleNamespace
from unittest.mock import patch
spec = importlib.util.spec_from_file_location("mysql_helper",sys.argv[1])
h = importlib.util.module_from_spec(spec)
spec.loader.exec_module(h)
real_openssl = sys.argv[2]

class MySQLTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = pathlib.Path(self.temp.name)
        self.store, self.calls, self.failure = None, [], None
        (self.directory/"credentials.env").write_text("unused-rds-fixture")

    def crypto(self,args):
        if self.failure == "certificate-expiring" and "-checkend" in args:
            raise h.app.ProvisionError("CERTIFICATE_RENEWAL_REQUIRED")
        result = subprocess.run([real_openssl,*args],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=60)
        if result.returncode: raise h.app.ProvisionError("TLS_FIXTURE_FAILED")
        return result.stdout.strip()

    def api(self,args,payload=None):
        self.calls.append((args,payload))
        if args == ["config","current-context"]:
            return "wrong" if self.failure == "context" else h.app.CONTEXT
        if args[:2] == ["get","namespace"]:
            if self.failure == "namespace": raise h.app.ProvisionError("DENIED")
            return "namespace/new-api"
        if args[0] == "get":
            if self.failure == "get": raise h.app.ProvisionError("DENIED")
            return json.dumps(self.store) if self.store else ""
        self.assertEqual(args,["create","--filename=-"])
        self.store = payload
        if self.failure == "create": raise h.app.ProvisionError("RESPONSE_LOST")
        return "created"

    def run_provision(self):
        original_open = os.open
        original_lstat = pathlib.Path.lstat
        def lstat_fixture(path):
            info = original_lstat(path)
            values = list(info)
            values[0] = (stat.S_IFDIR|0o700) if stat.S_ISDIR(info.st_mode) else (stat.S_IFMT(info.st_mode)|0o600)
            values[4] = 0
            return os.stat_result(values)
        def protected_fixture(path):
            if path.name in h.app.PUBLISHED_FILES:
                h.app.reconcile_publication(path)
            return path.read_text(encoding="utf-8")
        def open_fixture(path,flags,*args,**kwargs):
            if pathlib.Path(path) == self.directory:
                return original_open(self.directory/"credentials.env",os.O_RDWR)
            return original_open(path,flags,*args,**kwargs)
        # Windows simulates Linux ownership/file syscalls only; real OpenSSL,
        # file publication, literal contracts and Secret reconciliation execute.
        with patch.object(h,"DIRECTORY",self.directory), \
             patch.object(h.app,"DIRECTORY",self.directory), \
             patch.object(h.os,"geteuid",return_value=0,create=True), \
             patch.object(pathlib.Path,"lstat",autospec=True,side_effect=lstat_fixture), \
             patch.object(h.app,"protected_read",side_effect=protected_fixture), \
             patch.object(h.os,"fchmod",create=True), \
             patch.object(h.os,"O_DIRECTORY",getattr(os,"O_DIRECTORY",0),create=True), \
             patch.object(h.os,"O_NOFOLLOW",getattr(os,"O_NOFOLLOW",0),create=True), \
             patch.object(h.app,"sync_directory"), \
             patch.object(h.os,"open",side_effect=open_fixture), \
             patch.object(h,"openssl",side_effect=self.crypto), \
             patch.object(h.app,"kubectl",side_effect=self.api), redirect_stdout(io.StringIO()):
            h.provision()

    def test_interrupted_authoritative_package_reuses_exact_mysql_material(self):
        class Crash(BaseException): pass
        root = self.directory
        original_link = os.link
        for boundary in ("before-link","after-link"):
            with self.subTest(boundary=boundary):
                self.directory = root/boundary
                self.directory.mkdir()
                (self.directory/"credentials.env").write_text("unused-rds-fixture")
                self.store = None
                def link(source,target,**kwargs):
                    if pathlib.Path(target).name == h.BUNDLE and boundary == "before-link": raise Crash()
                    original_link(source,target,**kwargs)
                    if pathlib.Path(target).name == h.BUNDLE and boundary == "after-link": raise Crash()
                with patch.object(h.app.os,"link",side_effect=link),self.assertRaises(Crash):
                    self.run_provision()
                target = self.directory/h.BUNDLE
                stage = self.directory/f".new-api-publish.{h.BUNDLE}.pending"
                retained = (target if target.exists() else stage).read_bytes()
                expected = json.loads(retained)
                with patch.object(h,"generate_material",side_effect=AssertionError("MUST_NOT_REGENERATE")):
                    self.run_provision()
                self.assertEqual(target.read_bytes(),retained)
                self.assertEqual(target.stat().st_nlink,1)
                self.assertFalse(stage.exists())
                for name,content in expected.items():
                    self.assertEqual((self.directory/name).read_text(),content)
                self.assertEqual(h.app.base64.b64decode(self.store["data"]["app-password"]).decode(),expected["mysql-app-password"])
                self.assertEqual(h.app.base64.b64decode(self.store["data"]["root-password"]).decode(),expected["mysql-root-password"])
        self.directory = root

    def test_denial_then_generation_retry_drift_and_certificates(self):
        for failure in ("context","namespace","get"):
            self.failure = failure
            with self.assertRaises(h.app.ProvisionError): self.run_provision()
            self.assertEqual([p.name for p in self.directory.iterdir()],["credentials.env"])
        self.failure = "create"
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        snapshots = {p.name:p.read_bytes() for p in self.directory.iterdir()}
        self.assertEqual(set(snapshots),h.MATERIAL_KEYS|{h.BUNDLE,"credentials.env"})
        env = h.app.parse_literal((self.directory/"mysql.env").read_text(),h.app.MYSQL_KEYS)
        self.assertEqual(env["MYSQL_HOST"],h.HOST)
        self.assertIn("?tls=true&parseTime=true",env["SQL_DSN"])
        self.assertEqual(env["SSL_CERT_FILE"],(self.directory/"mysql-ca.pem").as_posix())
        self.assertNotIn((self.directory/"mysql-root-password").read_text(),env["SQL_DSN"])
        self.assertEqual(set(self.store["data"]),{"root-password","app-password","ca.pem","server.pem","server-key.pem"})
        self.assertNotIn("mysql-ca-key.pem",self.store["data"])
        ca, server = str(self.directory/"mysql-ca.pem"),str(self.directory/"mysql-server.pem")
        self.crypto(["verify","-CAfile",ca,"-verify_hostname",h.HOST,"-purpose","sslserver",server])
        with self.assertRaises(h.app.ProvisionError):
            self.crypto(["verify","-CAfile",ca,"-verify_hostname","wrong.example",server])
        cert = self.crypto(["x509","-in",server,"-text","-noout"])
        self.assertIn("Public-Key: (3072 bit)",cert)
        self.assertIn("DNS:"+h.HOST,cert)
        self.assertIn("CA:FALSE",cert)
        self.failure = None
        self.calls.clear()
        self.run_provision()
        self.assertFalse(any(args[0]=="create" for args,_ in self.calls))
        self.assertEqual({p.name:p.read_bytes() for p in self.directory.iterdir()},snapshots)
        original = self.store
        for mutation in ({"data":{}},{"immutable":False},{"metadata":{**original["metadata"],"labels":{}}}):
            self.store = {**original,**mutation}
            self.calls.clear()
            with self.assertRaises(h.app.ProvisionError): self.run_provision()
            self.assertFalse(any(args[0]=="create" for args,_ in self.calls))
            self.assertEqual({p.name:p.read_bytes() for p in self.directory.iterdir()},snapshots)
        self.store = original
        self.failure = "certificate-expiring"
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        self.assertEqual({p.name:p.read_bytes() for p in self.directory.iterdir()},snapshots)
        self.failure = None
        broken = json.loads(snapshots[h.BUNDLE])
        broken["mysql-server-key.pem"] = broken["mysql-ca-key.pem"]
        (self.directory/h.BUNDLE).write_text(json.dumps(broken))
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        (self.directory/h.BUNDLE).write_bytes(snapshots[h.BUNDLE])
        # Missing exported files are recovered from the fixed atomic package.
        (self.directory/"mysql.env").unlink()
        self.run_provision()
        self.assertEqual({p.name:p.read_bytes() for p in self.directory.iterdir()},snapshots)
        (self.directory/"mysql.env").write_text("drifted")
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        self.assertEqual((self.directory/"mysql.env").read_text(),"drifted")
        (self.directory/"mysql.env").write_bytes(snapshots["mysql.env"])
        (self.directory/h.BUNDLE).unlink()
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        self.assertFalse((self.directory/h.BUNDLE).exists())
        self.store = None
        with self.assertRaises(h.app.ProvisionError): self.run_provision()
        self.assertFalse((self.directory/h.BUNDLE).exists())
        self.assertEqual((self.directory/"credentials.env").read_text(),"unused-rds-fixture")

    def test_protection_publish_and_safe_subprocess(self):
        with patch.object(h.os,"geteuid",return_value=1,create=True),self.assertRaises(h.app.ProvisionError):
            h.provision()
        for mode,uid in ((stat.S_IFDIR|0o755,0),(stat.S_IFDIR|0o700,1),(stat.S_IFLNK|0o700,0)):
            with patch.object(h,"DIRECTORY",self.directory),patch.object(h.os,"geteuid",return_value=0,create=True), \
                 patch.object(pathlib.Path,"lstat",return_value=SimpleNamespace(st_mode=mode,st_uid=uid)), \
                 self.assertRaises(h.app.ProvisionError):
                h.provision()
        path = self.directory/"fixed"
        path.write_text("preserve")
        with patch.object(h.app,"protected_read",side_effect=lambda path:path.read_text()),self.assertRaises(h.app.ProvisionError):
            h.app.atomic_publish(path,"replace")
        self.assertEqual(path.read_text(),"preserve")
        # Reused application helper enforces root0600, nofollow and single-link inputs.
        source = pathlib.Path(sys.argv[1]).read_text()
        self.assertIn("app.protected_read",source)
        payload = h.app.resource("Secret",h.SECRET,{"root-password":"synthetic-only"})
        with patch.object(h.app.subprocess,"run",return_value=SimpleNamespace(returncode=0,stdout="ok",stderr="private")) as run:
            h.app.kubectl(["create","--filename=-"],payload)
            self.assertNotIn("synthetic-only",repr(run.call_args.args))
            self.assertEqual(json.loads(run.call_args.kwargs["input"]),payload)
        for args in ([],["--apply","extra"]):
            with patch.object(sys,"argv",["helper",*args]),patch.object(h,"provision") as provision,redirect_stderr(io.StringIO()):
                self.assertEqual(h.main(),1)
                provision.assert_not_called()
        with patch.object(sys,"argv",["helper","--apply"]),patch.object(h,"provision",side_effect=OSError("synthetic-private")), \
             redirect_stderr(io.StringIO()) as output:
            self.assertEqual(h.main(),1)
            self.assertNotIn("synthetic-private",output.getvalue())

unittest.main(argv=["fixtures"],verbosity=2)
`;
  const result = spawnSync("python", ["-B", "-", fileURLToPath(new URL("mysql-secret.py", directory)), openssl], {
    input: script, encoding: "utf8", timeout: 120000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stdout + result.stderr);
});
