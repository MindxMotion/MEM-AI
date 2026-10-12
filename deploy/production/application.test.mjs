import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const directory = fileURLToPath(new URL("./", import.meta.url));
const manifest = JSON.parse(readFileSync(join(directory, "application.json"), "utf8"));
const [pvc, policy, deployment, service] = manifest.items;
const pod = deployment.spec.template.spec;
const app = pod.containers[0];
const bash = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "bash";

test("bounded application resources, restricted security and local credential ownership", () => {
  assert.deepEqual(manifest.items.map(item => item.kind), ["PersistentVolumeClaim", "NetworkPolicy", "Deployment", "Service"]);
  for (const item of manifest.items) {
    assert.equal(item.metadata.namespace, "new-api");
    assert.equal(item.metadata.labels["app.kubernetes.io/part-of"], "new-api");
  }
  assert.equal(deployment.spec.replicas, 1);
  assert.deepEqual(deployment.spec.strategy, { type: "Recreate" });
  assert.deepEqual(deployment.spec.selector.matchLabels, { "app.kubernetes.io/name": "new-api" });
  assert.deepEqual(pod.nodeSelector, { "kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux" });
  assert.equal(pod.automountServiceAccountToken, false);
  assert.equal(pod.hostNetwork, false);
  assert.deepEqual(pod.securityContext, {
    runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001,
    fsGroupChangePolicy: "OnRootMismatch", seccompProfile: { type: "RuntimeDefault" },
  });
  assert.deepEqual(app.securityContext, {
    allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: ["ALL"] },
  });
  assert.deepEqual(app.resources, { requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "1000m", memory: "1Gi" } });
  assert.match(app.image, /^registry\.invalid\/new-api-subpath:37dcccf85f0579e9da2e565291100e5a8741c90b$/);
  assert.equal(app.imagePullPolicy, "Always");
  assert.deepEqual(pvc.spec.accessModes, ["ReadWriteOnce"]);
  assert.equal(pvc.spec.storageClassName, "local-path");
  assert.equal(pvc.spec.resources.requests.storage, "512Mi");
  assert.equal(app.workingDir, "/data");
  assert.deepEqual(pod.volumes, [
    { name: "data", persistentVolumeClaim: { claimName: "new-api-data" } },
    { name: "temporary", emptyDir: { sizeLimit: "256Mi" } },
    { name: "mysql-ca", configMap: { name: "new-api-mysql-ca", defaultMode: 0o444, items: [{ key: "mysql-ca.pem", path: "mysql-ca.pem" }] } },
  ]);
  assert.deepEqual(app.volumeMounts, [
    { name: "data", mountPath: "/data" }, { name: "temporary", mountPath: "/tmp" },
    { name: "mysql-ca", mountPath: "/run/new-api-ca", readOnly: true },
  ]);
  const env = Object.fromEntries(app.env.map(entry => [entry.name, entry.value ?? entry.valueFrom]));
  for (const key of ["SQL_DSN", "REDIS_CONN_STRING", "SESSION_SECRET", "CRYPTO_SECRET"]) {
    assert.deepEqual(env[key], { secretKeyRef: { name: "new-api-app-auth", key } });
  }
  for (const [key, value] of Object.entries({
    APP_BASE_PATH: "/new-api", SESSION_COOKIE_SECURE: "true",
    SESSION_COOKIE_TRUSTED_URL: "https://thedoorofai.com", SSL_CERT_FILE: "/run/new-api-ca/mysql-ca.pem",
    DEBUG: "false", DIFY_DEBUG: "false", GIN_MODE: "release", ENABLE_PPROF: "false",
    TLS_INSECURE_SKIP_VERIFY: "false", GENERATE_DEFAULT_TOKEN: "false",
    SQL_MAX_IDLE_CONNS: "2", SQL_MAX_OPEN_CONNS: "10", LOG_SQL_DSN: "",
  })) assert.equal(env[key], value);
  for (const key of ["ServerAddress", "SERVER_ADDRESS", "RegisterEnabled", "REGISTER_ENABLED", "MYSQL_PASSWORD"]) {
    assert.equal(env[key], undefined);
  }
  for (const probe of ["startupProbe", "readinessProbe", "livenessProbe"]) {
    assert.deepEqual(app[probe].httpGet, { path: "/api/status", port: "http" });
  }
  assert.equal(service.spec.type, "ClusterIP");
  assert.deepEqual(service.spec.selector, deployment.spec.selector.matchLabels);
  assert.deepEqual(service.spec.ports, [{ name: "http", port: 3000, targetPort: "http", protocol: "TCP" }]);
  assert.deepEqual(policy.spec.podSelector.matchLabels, deployment.spec.selector.matchLabels);
  assert.deepEqual(policy.spec.policyTypes, ["Ingress"]);
  assert.deepEqual(policy.spec.ingress, [{
    from: [{
      namespaceSelector: { matchLabels: { "kubernetes.io/metadata.name": "apisix" } },
      podSelector: { matchLabels: { "app.kubernetes.io/instance": "apisix", "app.kubernetes.io/name": "apisix" } },
    }], ports: [{ protocol: "TCP", port: 3000 }],
  }]);
  assert.doesNotMatch(JSON.stringify(manifest), /hostPort|hostPath|NodePort|LoadBalancer|latest|imagePullPolicy":"Never/);
});

test("actual manifest startup guard refuses SQLite, missing config and unsafe TLS without leaking values", () => {
  const fixture = mkdtempSync(join(tmpdir(), "new-api-app-"));
  try {
    const caPath = join(fixture, "ca.pem").replaceAll("\\", "/");
    writeFileSync(caPath, "fixture CA");
    const command = app.args[0].replaceAll("/run/new-api-ca/mysql-ca.pem", caPath)
      .replace("exec /new-api --port 3000 --log-dir /data/logs", "printf 'STARTED\\n'");
    const valid = {
      SQL_DSN: "new_api_app:fixture$(never-execute)@tcp(private.example:3306)/new_api?parseTime=true&tls=true",
      SESSION_SECRET: "a".repeat(64), CRYPTO_SECRET: "b".repeat(64),
      REDIS_CONN_STRING: `redis://new_api:${"c".repeat(64)}@new-api-redis.new-api.svc.cluster.local:6379/0`,
      APP_BASE_PATH: "/new-api", SSL_CERT_FILE: caPath,
    };
    const syntax = spawnSync(bash, ["-n"], { input: app.args[0], encoding: "utf8", timeout: 10000 });
    assert.ifError(syntax.error);
    assert.equal(syntax.status, 0, syntax.stderr);
    const cases = [
      {}, { SQL_DSN: "" }, { SQL_DSN: "local" }, { SQL_DSN: "postgres://fixture" },
      { SQL_DSN: valid.SQL_DSN.replace("tls=true", "tls=skip-verify") },
      { SQL_DSN: valid.SQL_DSN.replace("&tls=true", "") },
      { SQL_DSN: valid.SQL_DSN.replace("new_api?", "shared?") },
      { SQL_DSN: valid.SQL_DSN.replace("new_api_app:", "root:") },
      { SESSION_SECRET: "" }, { CRYPTO_SECRET: valid.SESSION_SECRET },
      { REDIS_CONN_STRING: "" }, { APP_BASE_PATH: "" }, { SSL_CERT_FILE: "/missing" },
    ];
    for (const [index, changes] of cases.entries()) {
      const result = spawnSync(bash, ["-ec", command], {
        env: { ...process.env, ...valid, ...changes }, encoding: "utf8", timeout: 10000,
      });
      assert.ifError(result.error);
      assert.equal(result.status, index === 0 ? 0 : 1);
      assert.equal(result.stdout, index === 0 ? "STARTED\n" : "");
      assert.equal(result.stderr, index === 0 ? "" : "NEW_API_REQUIRED_CONFIG_INVALID\n");
    }
  } finally { rmSync(fixture, { recursive: true, force: true }); }
});

test("Python stdlib provisioning fixtures: parsing, protected input, drift, no rotation and uncertain responses", () => {
  const script = String.raw`
import importlib.util, io, json, os, pathlib, stat, sys, tempfile, unittest
from contextlib import contextmanager, redirect_stdout, redirect_stderr
from types import SimpleNamespace
from unittest.mock import patch
spec = importlib.util.spec_from_file_location("helper", sys.argv[1])
h = importlib.util.module_from_spec(spec)
spec.loader.exec_module(h)

class ProvisionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = pathlib.Path(self.temp.name)
        self.mysql = dict(MYSQL_HOST="private.example", MYSQL_PORT="3306", MYSQL_DATABASE="new_api",
                          MYSQL_USER="new_api_app", MYSQL_PASSWORD="fixture$(not-executed)=!()",
                          MYSQL_SSL_MODE="VERIFY_IDENTITY", SSL_CERT_FILE=(self.directory/"rds-ca.pem").as_posix())
        self.mysql["SQL_DSN"] = "new_api_app:" + self.mysql["MYSQL_PASSWORD"] + "@tcp(private.example:3306)/new_api?tls=true&parseTime=true&charset=utf8mb4"
        self.redis = dict(REDIS_USERNAME="new_api", REDIS_PASSWORD="c"*64)
        self.redis["REDIS_CONN_STRING"] = "redis://new_api:" + self.redis["REDIS_PASSWORD"] + "@new-api-redis.new-api.svc.cluster.local:6379/0"
        for name, values in (("mysql.env", self.mysql), ("redis.env", self.redis)):
            (self.directory / name).write_text("".join(k+"="+v+"\n" for k,v in values.items()), encoding="utf-8")
        (self.directory / "rds-ca.pem").write_text("fixture-ca")
        self.store, self.calls, self.failure = {}, [], None

    def api(self, args, payload=None):
        self.calls.append((args, payload))
        if args == ["config", "current-context"]:
            return "wrong" if self.failure == "context" else h.CONTEXT
        if args[:2] == ["get", "namespace"]:
            if self.failure == "namespace": raise h.ProvisionError("DENIED")
            return "namespace/new-api"
        if args[0] == "get":
            if self.failure == "get": raise h.ProvisionError("DENIED")
            kind = "Secret" if args[1] == "secret" else "ConfigMap"
            return json.dumps(self.store[(kind, args[2])]) if (kind, args[2]) in self.store else ""
        self.assertEqual(args, ["create", "--filename=-"])
        self.store[(payload["kind"], payload["metadata"]["name"])] = payload
        if self.failure == "create": raise h.ProvisionError("RESPONSE_LOST")
        return "created"

    def run_provision(self, credentials_file=None):
        # Windows fixtures simulate Linux ownership/SSL checks; real parsing,
        # atomic key publication and API JSON reconciliation are exercised.
        original_open = os.open
        original_lstat = pathlib.Path.lstat
        def lstat_fixture(path):
            info = original_lstat(path)
            values = list(info)
            values[0] = (stat.S_IFDIR|0o700) if stat.S_ISDIR(info.st_mode) else (stat.S_IFMT(info.st_mode)|0o600)
            values[4] = 0
            return os.stat_result(values)
        def protected_fixture(path):
            if path.name in h.PUBLISHED_FILES:
                h.reconcile_publication(path)
            return path.read_text(encoding="utf-8")
        def open_fixture(path, flags, *args, **kwargs):
            if pathlib.Path(path) == self.directory:
                return original_open(self.directory / "rds-ca.pem", os.O_RDWR)
            return original_open(path, flags, *args, **kwargs)
        with patch.object(h, "DIRECTORY", self.directory), \
             patch.object(h.os, "geteuid", return_value=0, create=True), \
             patch.object(pathlib.Path, "lstat", autospec=True, side_effect=lstat_fixture), \
             patch.object(h, "protected_read", side_effect=protected_fixture), \
             patch.object(h.ssl, "SSLContext") as tls, \
             patch.object(h, "kubectl", side_effect=self.api), \
             patch.object(h.os, "fchmod", create=True), \
             patch.object(h.os, "O_DIRECTORY", getattr(os, "O_DIRECTORY", 0), create=True), \
             patch.object(h.os, "O_NOFOLLOW", getattr(os, "O_NOFOLLOW", 0), create=True), \
             patch.object(h, "sync_directory"), \
             patch.object(h.os, "open", side_effect=open_fixture), \
             redirect_stdout(io.StringIO()):
            h.provision(credentials_file)

    @contextmanager
    def publication_fixture(self):
        # Real file bytes, file fsync, hardlinks and inode/link counts.
        # Windows mocks POSIX owner/mode/nofollow and directory fsync only.
        original_lstat, original_fstat = pathlib.Path.lstat, os.fstat
        def metadata(info):
            values = list(info)
            values[0] = (stat.S_IFDIR|0o700) if stat.S_ISDIR(info.st_mode) else (stat.S_IFMT(info.st_mode)|0o600)
            values[4] = 0
            return os.stat_result(values)
        with patch.object(h,"DIRECTORY",self.directory), \
             patch.object(pathlib.Path,"lstat",autospec=True,side_effect=lambda path:metadata(original_lstat(path))), \
             patch.object(h.os,"fstat",side_effect=lambda fd:metadata(original_fstat(fd))), \
             patch.object(h.os,"fchmod",create=True), \
             patch.object(h.os,"O_NOFOLLOW",getattr(os,"O_NOFOLLOW",0),create=True), \
             patch.object(h.os,"O_NONBLOCK",getattr(os,"O_NONBLOCK",0),create=True), \
             patch.object(h,"sync_directory") as synced:
            yield synced

    def test_publication_interruption_boundaries_reuse_exact_app_keys(self):
        class Crash(BaseException): pass
        path = self.directory/"application.env"
        original_link, original_unlink, original_fsync = os.link, os.unlink, os.fsync
        for boundary in ("file-fsync","stage-directory-fsync","before-link","after-link",
                         "linked-directory-fsync","before-unlink","after-unlink","final-directory-fsync"):
            with self.subTest(boundary=boundary), self.publication_fixture() as synced:
                stage = h.publication_stage(path)
                events, sync_count = [], 0
                def link(source,target,**kwargs):
                    events.append("link")
                    if boundary == "before-link": raise Crash()
                    original_link(source,target,**kwargs)
                    if boundary == "after-link": raise Crash()
                def unlink(target):
                    events.append("unlink")
                    if boundary == "before-unlink": raise Crash()
                    original_unlink(target)
                    if boundary == "after-unlink": raise Crash()
                def fsync(fd):
                    original_fsync(fd)
                    events.append("file-fsync")
                    if boundary == "file-fsync": raise Crash()
                def sync_directory(directory):
                    nonlocal sync_count
                    sync_count += 1
                    events.append("directory-fsync")
                    if (boundary,sync_count) in (("stage-directory-fsync",1),
                                                ("linked-directory-fsync",2),("final-directory-fsync",3)):
                        raise Crash()
                synced.side_effect = sync_directory
                with patch.object(h.os,"link",side_effect=link), \
                     patch.object(h.os,"unlink",side_effect=unlink), \
                     patch.object(h.os,"fsync",side_effect=fsync), self.assertRaises(Crash):
                    h.persist_app_keys(path)
                retained = (path if path.exists() else stage).read_bytes()
                expected = h.parse_literal(retained.decode(),h.APP_KEYS)
                synced.side_effect = None
                # Model a crash after unlink where the last directory fsync did
                # not happen and the known staging name reappears after reboot.
                if boundary == "after-unlink":
                    original_link(path,stage)
                with patch.object(h.secrets,"token_hex",side_effect=AssertionError("MUST_NOT_REGENERATE")):
                    if path.exists():
                        recovered = h.parse_literal(h.protected_read(path),h.APP_KEYS)
                    else:
                        recovered = h.persist_app_keys(path)
                self.assertEqual(recovered,expected)
                self.assertEqual(path.read_bytes(),retained)
                self.assertEqual(path.stat().st_nlink,1)
                self.assertFalse(stage.exists())
                # Equal retry is a no-op; unequal retry must not overwrite.
                h.atomic_publish(path,retained.decode())
                with self.assertRaises(h.ProvisionError): h.atomic_publish(path,"different")
                self.assertEqual(path.read_bytes(),retained)
                if boundary == "final-directory-fsync":
                    self.assertEqual(events,["file-fsync","directory-fsync","link","directory-fsync","unlink","directory-fsync"])
            path.unlink()

    def test_publication_rejects_unknown_hardlinks_and_unrelated_staging(self):
        path = self.directory/"application.env"
        with self.publication_fixture():
            h.atomic_publish(path,"fixed-fixture")
            stage = h.publication_stage(path)
            unknown = self.directory/"unknown-hardlink"
            os.link(path,unknown)
            with self.assertRaises(h.ProvisionError): h.protected_read(path)
            self.assertTrue(unknown.exists())
            unknown.unlink()
            os.link(path,stage)
            os.link(path,unknown)
            with self.assertRaises(h.ProvisionError): h.protected_read(path)
            self.assertTrue(stage.exists() and unknown.exists(),"unknown link must block cleanup")
            unknown.unlink()
            self.assertEqual(h.protected_read(path),"fixed-fixture")
            self.assertFalse(stage.exists())
            stage.write_text("unrelated-fixture")
            with self.assertRaises(h.ProvisionError): h.protected_read(path)
            self.assertEqual(stage.read_text(),"unrelated-fixture")
            self.assertEqual(path.read_text(),"fixed-fixture")
            stage.unlink()
            path.unlink()
            stage.write_text("incomplete")
            with patch.object(h.secrets,"token_hex",side_effect=AssertionError("MUST_NOT_REGENERATE")),self.assertRaises(h.ProvisionError):
                h.persist_app_keys(path)
            self.assertEqual(stage.read_text(),"incomplete")
            self.assertFalse(path.exists())
            stage.unlink()
            original_link = os.link
            def competing_target(source,target,**kwargs):
                target.write_text("concurrent-fixture")
                original_link(source,target,**kwargs)
            with patch.object(h.os,"link",side_effect=competing_target),self.assertRaises(FileExistsError):
                h.atomic_publish(path,"pending-fixture")
            self.assertEqual(path.read_text(),"concurrent-fixture")
            self.assertEqual(stage.read_text(),"pending-fixture")
            with self.assertRaises(h.ProvisionError): h.protected_read(path)
            self.assertTrue(stage.exists(),"unrelated staging inode must not be deleted")

    def test_literal_and_input_denial(self):
        content = (self.directory / "mysql.env").read_text()
        self.assertEqual(h.parse_literal(content, h.MYSQL_KEYS), self.mysql)
        for content in ("X=x\nX=y", "SESSION_SECRET=", "export SESSION_SECRET=x",
                        "SESSION_SECRET=x\nCRYPTO_SECRET=y\nSESSION_SECRET=z",
                        "SESSION_SECRET=x\x00\nCRYPTO_SECRET=y",
                        "SESSION_SECRET=x\vCRYPTO_SECRET=y",
                        "SESSION_SECRET=x\nCRYPTO_SECRET=y\nUNKNOWN=z"):
            with self.subTest(content=content), self.assertRaises(h.ProvisionError):
                h.parse_literal(content, h.APP_KEYS)
        h.validate_credentials(self.mysql, self.redis)
        for key, value in (("MYSQL_DATABASE","shared"), ("MYSQL_USER","root"),
                           ("MYSQL_SSL_MODE","skip-verify"),
                           ("SQL_DSN",self.mysql["SQL_DSN"]+"&tls=skip-verify"),
                           ("SQL_DSN",self.mysql["SQL_DSN"].replace("tls=true","tls=false")),
                           ("SQL_DSN",self.mysql["SQL_DSN"]+"&multiStatements=true")):
            with self.subTest(key=key), self.assertRaises(h.ProvisionError):
                h.validate_credentials({**self.mysql, key:value}, self.redis)
        for key, value in (("REDIS_USERNAME","default"), ("REDIS_PASSWORD","short"),
                           ("REDIS_CONN_STRING", self.redis["REDIS_CONN_STRING"][:-1]+"1")):
            with self.subTest(key=key), self.assertRaises(h.ProvisionError):
                h.validate_credentials(self.mysql, {**self.redis,key:value})

    def test_file_boundary_denies_mode_owner_links_and_nonregular(self):
        for mode, uid, links in ((stat.S_IFREG|0o600,0,1), (stat.S_IFREG|0o644,0,1),
                                (stat.S_IFREG|0o600,1,1), (stat.S_IFREG|0o600,0,2),
                                (stat.S_IFDIR|0o600,0,1)):
            fd = os.open(self.directory / "rds-ca.pem", os.O_RDONLY)
            with patch.object(h.os, "O_NOFOLLOW", 0x20000, create=True), \
                 patch.object(h.os, "O_NONBLOCK", 0x800, create=True), \
                 patch.object(h.os, "open", return_value=fd) as opened, \
                 patch.object(h.os, "fstat", return_value=SimpleNamespace(st_mode=mode,st_uid=uid,st_nlink=links)):
                if mode == stat.S_IFREG|0o600 and uid == 0 and links == 1:
                    self.assertEqual(h.protected_read("fixture"), "fixture-ca")
                else:
                    with self.assertRaises(h.ProvisionError): h.protected_read("fixture")
                self.assertTrue(opened.call_args.args[1] & 0x20000)
        with patch.object(h.os, "geteuid", return_value=1, create=True), self.assertRaises(h.ProvisionError):
            h.provision()
        for mode, uid in ((stat.S_IFDIR|0o755,0),(stat.S_IFDIR|0o700,1),(stat.S_IFLNK|0o700,0)):
            with patch.object(h, "DIRECTORY", self.directory), \
                 patch.object(h.os,"geteuid",return_value=0,create=True), \
                 patch.object(pathlib.Path,"lstat",return_value=SimpleNamespace(st_mode=mode,st_uid=uid)), \
                 self.assertRaises(h.ProvisionError):
                h.provision()

    def test_create_retry_and_no_rotation(self):
        self.run_provision()
        package = (self.directory / "application.env").read_bytes()
        keys = h.parse_literal(package.decode(), h.APP_KEYS)
        self.assertNotEqual(keys["SESSION_SECRET"], keys["CRYPTO_SECRET"])
        self.assertEqual(len(keys["SESSION_SECRET"]),64)
        self.assertEqual(set(self.store), {("Secret","new-api-app-auth"),("ConfigMap","new-api-mysql-ca")})
        self.assertEqual(set(self.store[("Secret","new-api-app-auth")]["data"]), h.APP_KEYS|{"SQL_DSN","REDIS_CONN_STRING"})
        self.calls.clear()
        self.run_provision()
        self.assertEqual((self.directory / "application.env").read_bytes(), package)
        self.assertFalse(any(args[0] == "create" for args,_ in self.calls))
        self.assertEqual(sorted(p.name for p in self.directory.iterdir()),
                         ["application.env","mysql.env","rds-ca.pem","redis.env"])
        self.assertEqual(h.parse_literal((self.directory/"mysql.env").read_text(),h.MYSQL_KEYS),self.mysql)
        self.assertEqual(h.parse_literal((self.directory/"redis.env").read_text(),h.REDIS_KEYS),self.redis)
        self.assertEqual((self.directory/"rds-ca.pem").read_text(),"fixture-ca")

    def test_missing_mysql_package_never_falls_back_to_old_rds(self):
        (self.directory/"credentials.env").write_text("unused-rds-fixture")
        (self.directory/"mysql.env").unlink()
        with self.assertRaises(OSError): self.run_provision()
        self.assertFalse(self.calls)
        self.assertFalse((self.directory/"application.env").exists())
        self.assertEqual((self.directory/"credentials.env").read_text(),"unused-rds-fixture")

    def test_explicit_credentials_path_selection_and_drift(self):
        alternate = {**self.mysql, "MYSQL_HOST":"new-api-mysql.new-api.svc.cluster.local",
                     "SSL_CERT_FILE":(self.directory/"mysql-ca.pem").as_posix()}
        alternate["SQL_DSN"] = self.mysql["SQL_DSN"].replace("private.example",alternate["MYSQL_HOST"])
        path = self.directory/"alternate-mysql.env"
        path.write_text("".join(k+"="+v+"\n" for k,v in alternate.items()))
        (self.directory/"mysql-ca.pem").write_text("fixture-dedicated-ca")
        self.run_provision(path.as_posix())
        actual = self.store[("Secret","new-api-app-auth")]
        self.assertEqual(actual["data"]["SQL_DSN"],h.base64.b64encode(alternate["SQL_DSN"].encode()).decode())
        self.assertEqual(self.store[("ConfigMap","new-api-mysql-ca")]["data"],{"mysql-ca.pem":"fixture-dedicated-ca"})
        package = (self.directory/"application.env").read_bytes()
        self.calls.clear()
        self.run_provision(path.as_posix())
        self.assertFalse(any(args[0]=="create" for args,_ in self.calls))
        with self.assertRaises(h.ProvisionError): self.run_provision()
        self.assertEqual((self.directory/"application.env").read_bytes(),package)
        self.assertEqual(path.read_text(),"".join(k+"="+v+"\n" for k,v in alternate.items()))

    def test_explicit_path_denial_and_cli_validation(self):
        parent = self.directory.as_posix()
        for value in ("mysql.env", parent+"/../mysql.env", parent+"/./mysql.env",
                      parent+"//mysql.env", parent+"/nested/mysql.env", parent+"/mysql.txt",
                      "/outside/mysql.env", parent+"/mysql.env\n", parent+"/mysql.env/"):
            with self.subTest(value=value), self.assertRaises(h.ProvisionError):
                self.run_provision(value)
        self.assertFalse(self.calls)
        self.assertFalse((self.directory/"application.env").exists())
        # Selected CA cannot escape the protected directory either.
        for value in ("/outside/ca.pem", parent+"/../ca.pem", parent+"/ca.txt"):
            with patch.object(h,"DIRECTORY",self.directory), self.assertRaises(h.ProvisionError):
                h.protected_package_path(value,".pem")
        for args, expected in ((["--apply"],None),
                               (["--apply","--credentials-file",parent+"/mysql.env"],parent+"/mysql.env")):
            with patch.object(sys,"argv",["helper",*args]), patch.object(h,"provision") as provision:
                self.assertEqual(h.main(),0)
                provision.assert_called_once_with(expected)
        for args in ([], ["--credentials-file",parent+"/mysql.env"],
                     ["--apply","--credentials-file"], ["--apply","--credentials-file","x","--credentials-file","y"],
                     ["--apply","--unknown","x"]):
            with patch.object(sys,"argv",["helper",*args]), patch.object(h,"provision") as provision, redirect_stderr(io.StringIO()):
                self.assertEqual(h.main(),1)
                provision.assert_not_called()

    def test_resource_drift_denies_before_writes(self):
        self.run_provision()
        package = (self.directory/"application.env").read_bytes()
        original = self.store[("Secret","new-api-app-auth")]
        for mutation in ({"data":{}}, {"immutable":False}, {"metadata":{**original["metadata"],"labels":{}}},
                         {"metadata":{**original["metadata"],"namespace":"other"}}, {"type":"other"}):
            self.store[("Secret","new-api-app-auth")] = {**original,**mutation}
            self.calls.clear()
            with self.assertRaises(h.ProvisionError): self.run_provision()
            self.assertFalse(any(args[0]=="create" for args,_ in self.calls))
            self.assertEqual((self.directory/"application.env").read_bytes(),package)
        self.store[("Secret","new-api-app-auth")] = original
        (self.directory/"application.env").unlink()
        with self.assertRaises(h.ProvisionError): self.run_provision()
        self.assertFalse((self.directory/"application.env").exists())

    def test_ca_drift_invalid_package_and_atomic_no_overwrite(self):
        self.store[("ConfigMap","new-api-mysql-ca")] = h.resource("ConfigMap","new-api-mysql-ca",{"mysql-ca.pem":"changed"})
        with self.assertRaises(h.ProvisionError): self.run_provision()
        self.assertFalse((self.directory/"application.env").exists())
        self.assertFalse(any(args[0]=="create" for args,_ in self.calls))
        self.store.clear()
        path = self.directory/"application.env"
        for content in ("SESSION_SECRET=short\nCRYPTO_SECRET=short",
                        "SESSION_SECRET="+"a"*64+"\nCRYPTO_SECRET="+"a"*64,
                        "SESSION_SECRET="+"a"*64+"\nCRYPTO_SECRET="+"b"*64+"\nSESSION_SECRET=duplicate"):
            path.write_text(content)
            with self.assertRaises(h.ProvisionError): self.run_provision()
            self.assertEqual(path.read_text(),content)
        # Exclusive hard-link publication must never overwrite even a concurrent file.
        with patch.object(h.os,"fchmod",create=True), self.assertRaises(FileExistsError):
            h.persist_app_keys(path)
        self.assertEqual(path.read_text(),content)
        self.assertFalse(any(p.name.startswith(".new-api-publish.") for p in self.directory.iterdir()))

    def test_failure_preserves_keys_and_denials_do_not_generate(self):
        for failure in ("context","namespace","get"):
            self.failure = failure
            with self.assertRaises(h.ProvisionError): self.run_provision()
            self.assertFalse((self.directory/"application.env").exists())
        self.failure = "create"
        with self.assertRaises(h.ProvisionError): self.run_provision()
        package = (self.directory/"application.env").read_bytes()
        self.failure = None
        self.run_provision()
        self.assertEqual((self.directory/"application.env").read_bytes(),package)
        # Simulate API accepting app Secret then losing its response.
        del self.store[("Secret","new-api-app-auth")]
        self.failure = "create"
        with self.assertRaises(h.ProvisionError): self.run_provision()
        self.assertEqual((self.directory/"application.env").read_bytes(),package)
        self.failure = None
        self.calls.clear()
        self.run_provision()
        self.assertFalse(any(args[0]=="create" for args,_ in self.calls))

    def test_subprocess_stdin_and_sanitized_errors(self):
        payload = h.resource("Secret","new-api-app-auth",{"SESSION_SECRET":"fixture-sensitive"})
        with patch.object(h.subprocess,"run",return_value=SimpleNamespace(returncode=0,stdout="ok",stderr="private")) as run:
            h.kubectl(["create","--filename=-"],payload)
            self.assertNotIn("fixture-sensitive",repr(run.call_args.args))
            self.assertEqual(json.loads(run.call_args.kwargs["input"]),payload)
            self.assertEqual(run.call_args.kwargs["stderr"],h.subprocess.PIPE)
        for error in (h.ProvisionError("RESOURCE_DRIFT_RECONCILE_PRIVATELY_NO_OVERWRITE"),
                      OSError("fixture-sensitive"), ValueError("fixture-sensitive"),
                      h.subprocess.TimeoutExpired("private",30)):
            output = io.StringIO()
            with patch.object(sys,"argv",["helper","--apply"]), patch.object(h,"provision",side_effect=error), redirect_stderr(output):
                self.assertEqual(h.main(),1)
            self.assertNotIn("fixture-sensitive",output.getvalue())
            self.assertNotIn("private",output.getvalue().replace("privately","").replace("private keys",""))

unittest.main(argv=["fixtures"],verbosity=2)
`;
  const result = spawnSync("python", ["-B", "-", join(directory, "application-secret.py")], {
    input: script, encoding: "utf8", timeout: 30000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stdout + result.stderr);
});
