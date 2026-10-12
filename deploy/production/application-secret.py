"""Explicit root-only provisioning; never updates credentials or starts workloads."""

import base64
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import stat
import subprocess
import sys
from urllib.parse import parse_qsl


CONTEXT = "kubernetes-admin@cluster.local"
NAMESPACE = "new-api"
DIRECTORY = Path("/root/new-api-deploy")
MYSQL_KEYS = {
    "MYSQL_HOST", "MYSQL_PORT", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD",
    "MYSQL_SSL_MODE", "SQL_DSN", "SSL_CERT_FILE",
}
REDIS_KEYS = {"REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_CONN_STRING"}
APP_KEYS = {"SESSION_SECRET", "CRYPTO_SECRET"}
LABELS = {"app.kubernetes.io/part-of": "new-api", "app.kubernetes.io/managed-by": "new-api-credential-helper"}
PUBLISHED_FILES = {
    "application.env", "mysql-package.json", "mysql.env", "mysql-root-password",
    "mysql-app-password", "mysql-ca.pem", "mysql-ca-key.pem",
    "mysql-server.pem", "mysql-server-key.pem",
}


class ProvisionError(Exception):
    """Only fixed, non-sensitive error codes may cross this boundary."""


def protected_package_path(value, suffix):
    # Only canonical absolute direct children of the already protected directory.
    # Do not resolve/follow a symlink or normalize traversal into an accepted path.
    path = Path(value)
    if (not path.is_absolute() or path.parent != DIRECTORY
            or path.as_posix() != value
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]*" + re.escape(suffix), path.name)):
        raise ProvisionError("PROTECTED_PACKAGE_PATH_INVALID")
    return path


def protected_read(path):
    path = Path(path)
    if path.name in PUBLISHED_FILES:
        reconcile_publication(path)
    # Root-owned 0700 parent is checked before any file operation. O_NOFOLLOW
    # plus fstat closes the leaf symlink/stat-open race on the production Linux host.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise ProvisionError("PROTECTED_FILE_OWNERSHIP_MODE_OR_TYPE_INVALID")
        content = stream.read(65537)
        if len(content) > 65536:
            raise ProvisionError("PROTECTED_FILE_TOO_LARGE")
        return content.decode("utf-8")


def publication_stage(path):
    path = Path(path)
    if path.parent != DIRECTORY or path.name not in PUBLISHED_FILES:
        raise ProvisionError("PUBLICATION_PATH_NOT_OWNED")
    info = path.parent.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ProvisionError("PROTECTED_DIRECTORY_INVALID")
    return path.with_name(f".new-api-publish.{path.name}.pending")


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o700):
            raise ProvisionError("PROTECTED_DIRECTORY_INVALID")
        os.fsync(fd)
    finally:
        os.close(fd)


def reconcile_publication(path):
    stage = publication_stage(path)
    if not (stage.exists() or stage.is_symlink()) or not (path.exists() or path.is_symlink()):
        return
    # Only the reserved staging name for this exact approved target is eligible.
    # Both entries must be the same regular root0600 inode with exactly two links.
    # An unknown third link, wrong inode, symlink or unrelated file stops untouched.
    target_info, stage_info = path.lstat(), stage.lstat()
    for info in (target_info, stage_info):
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 2):
            raise ProvisionError("PUBLICATION_LINK_STATE_INVALID")
    if (target_info.st_dev, target_info.st_ino) != (stage_info.st_dev, stage_info.st_ino):
        raise ProvisionError("PUBLICATION_STAGING_INODE_MISMATCH")
    # Make the permanent name durable before removing its only known staging link.
    sync_directory(path.parent)
    os.unlink(stage)
    sync_directory(path.parent)
    final = path.lstat()
    if (final.st_dev, final.st_ino, final.st_nlink) != (target_info.st_dev, target_info.st_ino, 1):
        raise ProvisionError("PUBLICATION_CHANGED_DURING_RECONCILIATION")


def pending_publication(path):
    stage = publication_stage(path)
    if not (stage.exists() or stage.is_symlink()):
        return None
    # No target yet: only a single-link protected staging file can be considered.
    # Callers validate their literal keys/JSON/cryptographic material before use.
    return protected_read(stage)


def atomic_publish(path, content):
    path = Path(path)
    stage = publication_stage(path)
    if path.exists() or path.is_symlink():
        if protected_read(path) != content:
            raise ProvisionError("PRIVATE_PACKAGE_DRIFT_NO_OVERWRITE")
        return
    pending = pending_publication(path)
    if pending is not None:
        if pending != content:
            raise ProvisionError("PUBLICATION_PENDING_CONTENT_MISMATCH_NO_OVERWRITE")
        # A completed but interrupted staging file is reused, never regenerated.
        fd = os.open(stage, os.O_RDWR | os.O_NOFOLLOW)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    else:
        fd = os.open(stage, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        # Intentionally preserve staging on interruption, including a partial
        # write. Invalid partial contents fail closed on retry, not regeneration.
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
    sync_directory(path.parent)
    os.link(stage, path, follow_symlinks=False)  # Atomic no-replace publication.
    reconcile_publication(path)  # Includes directory fsync AFTER final unlink.


def parse_literal(content, required):
    values = {}
    for line in content.split("\n"):
        line = line.removesuffix("\r")
        if not line:
            continue
        key, separator, value = line.partition("=")
        if (not separator or key not in required or key in values or not value
                or any(ord(char) < 32 or ord(char) == 127 for char in value)):
            raise ProvisionError("LITERAL_PACKAGE_INVALID")
        values[key] = value
    if values.keys() != required:
        raise ProvisionError("LITERAL_PACKAGE_KEYS_MISSING")
    return values


def validate_credentials(mysql, redis):
    if (mysql["MYSQL_DATABASE"] != "new_api" or mysql["MYSQL_USER"] != "new_api_app"
            or mysql["MYSQL_PORT"] != "3306"
            or not re.fullmatch(r"[a-zA-Z0-9.-]+", mysql["MYSQL_HOST"])
            or mysql["MYSQL_SSL_MODE"] not in {"true", "VERIFY_IDENTITY", "verify-full"}):
        raise ProvisionError("MYSQL_IDENTITY_OR_TLS_CONFIG_INVALID")
    prefix = (f'new_api_app:{mysql["MYSQL_PASSWORD"]}@tcp('
              f'{mysql["MYSQL_HOST"]}:3306)/new_api?')
    if not mysql["SQL_DSN"].startswith(prefix):
        raise ProvisionError("MYSQL_DSN_IDENTITY_DRIFT")
    query = parse_qsl(mysql["SQL_DSN"][len(prefix):], keep_blank_values=True, strict_parsing=True)
    params = dict(query)
    if (len(query) != len(params) or params.get("tls") != "true"
            or not params.keys() <= {"tls", "parseTime", "charset", "loc"}
            or params.get("parseTime", "true") != "true"
            or params.get("charset", "utf8mb4") != "utf8mb4"
            or params.get("loc", "Local") not in {"Local", "UTC"}):
        raise ProvisionError("MYSQL_DSN_TLS_OR_OPTIONS_INVALID")
    # This is the literal format created by redis-secret.sh, not shell syntax.
    if (redis["REDIS_USERNAME"] != "new_api"
            or not re.fullmatch(r"[a-f0-9]{64}", redis["REDIS_PASSWORD"])
            or redis["REDIS_CONN_STRING"] != (
                f'redis://new_api:{redis["REDIS_PASSWORD"]}'
                '@new-api-redis.new-api.svc.cluster.local:6379/0')):
        raise ProvisionError("REDIS_IDENTITY_OR_URL_DRIFT")


def kubectl(arguments, payload=None):
    # No values in argv; neither API stderr nor returned Secret JSON is printed.
    result = subprocess.run(
        ["kubectl", "--context", CONTEXT, "--namespace", NAMESPACE, *arguments],
        input=None if payload is None else json.dumps(payload),
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30,
        check=False,
    )
    if result.returncode:
        raise ProvisionError("KUBECTL_FAILED_RECONCILE_PRIVATELY_NO_ROTATION")
    return result.stdout.strip()


def existing_resource(kind, name):
    output = kubectl(["get", kind, name, "--ignore-not-found", "--output=json"])
    if not output:
        return None
    result = json.loads(output)
    if not isinstance(result, dict):
        raise ProvisionError("RESOURCE_RESPONSE_INVALID")
    return result


def resource(kind, name, values):
    result = {
        "apiVersion": "v1", "kind": kind,
        "metadata": {"name": name, "namespace": NAMESPACE, "labels": LABELS},
        "immutable": True,
        "data": values,
    }
    if kind == "Secret":
        result["type"] = "Opaque"
        result["data"] = {
            key: base64.b64encode(value.encode("utf-8")).decode("ascii")
            for key, value in values.items()
        }
    return result


def require_equal(existing, expected):
    if existing is None:
        return
    metadata = existing.get("metadata", {})
    if not isinstance(metadata, dict) or not isinstance(metadata.get("labels", {}), dict):
        raise ProvisionError("RESOURCE_METADATA_INVALID")
    if (any(existing.get(key) != expected[key] for key in ("apiVersion", "kind", "immutable", "data"))
            or any(metadata.get(key) != expected["metadata"][key] for key in ("name", "namespace"))
            or any(metadata.get("labels", {}).get(key) != value for key, value in LABELS.items())
            or (expected["kind"] == "Secret" and existing.get("type") != "Opaque")):
        raise ProvisionError("RESOURCE_DRIFT_RECONCILE_PRIVATELY_NO_OVERWRITE")


def persist_app_keys(path):
    if path.exists() or path.is_symlink():
        raise FileExistsError("APP_KEYS_ALREADY_EXIST")
    pending = pending_publication(path)
    if pending is None:
        values = {key: secrets.token_hex(32) for key in sorted(APP_KEYS)}
        content = "".join(f"{key}={value}\n" for key, value in values.items())
    else:
        content = pending
        values = parse_literal(content, APP_KEYS)
        if (any(not re.fullmatch(r"[a-f0-9]{64}", value) for value in values.values())
                or values["SESSION_SECRET"] == values["CRYPTO_SECRET"]):
            raise ProvisionError("PENDING_APP_KEYS_INVALID_NO_REGENERATION")
    # Publish without replacement; retain the package even if later API creation
    # fails or its response is lost. A retry must reuse these exact keys.
    atomic_publish(path, content)
    return values


def provision(credentials_file=None):
    if os.geteuid() != 0:
        raise ProvisionError("ROOT_REQUIRED")
    info = DIRECTORY.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != 0
            or stat.S_IMODE(info.st_mode) != 0o700):
        raise ProvisionError("PROTECTED_DIRECTORY_INVALID")
    credentials_path = protected_package_path(
        credentials_file if credentials_file is not None else (DIRECTORY / "mysql.env").as_posix(),
        ".env",
    )
    mysql = parse_literal(protected_read(credentials_path), MYSQL_KEYS)
    redis = parse_literal(protected_read(DIRECTORY / "redis.env"), REDIS_KEYS)
    validate_credentials(mysql, redis)
    ca = protected_read(protected_package_path(mysql["SSL_CERT_FILE"], ".pem"))
    ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT).load_verify_locations(cadata=ca)
    if kubectl(["config", "current-context"]) != CONTEXT:
        raise ProvisionError("CONTEXT_INVALID")
    kubectl(["get", "namespace", NAMESPACE, "--output=name"])
    app_existing = existing_resource("secret", "new-api-app-auth")
    ca_existing = existing_resource("configmap", "new-api-mysql-ca")
    ca_expected = resource("ConfigMap", "new-api-mysql-ca", {"mysql-ca.pem": ca})
    require_equal(ca_existing, ca_expected)
    path = DIRECTORY / "application.env"
    if path.exists() or path.is_symlink():
        keys = parse_literal(protected_read(path), APP_KEYS)
        if (any(not re.fullmatch(r"[a-f0-9]{64}", value) for value in keys.values())
                or keys["SESSION_SECRET"] == keys["CRYPTO_SECRET"]):
            raise ProvisionError("FIXED_APP_KEYS_INVALID")
    else:
        if app_existing is not None:
            raise ProvisionError("APP_SECRET_EXISTS_WITHOUT_PRIVATE_KEYS_NO_ROTATION")
        keys = persist_app_keys(path)
    expected = resource("Secret", "new-api-app-auth", {
        **keys, "SQL_DSN": mysql["SQL_DSN"], "REDIS_CONN_STRING": redis["REDIS_CONN_STRING"],
    })
    # Preflight all existing resources before any cluster write.
    require_equal(app_existing, expected)
    for existing, wanted in ((ca_existing, ca_expected), (app_existing, expected)):
        if existing is None:
            kubectl(["create", "--filename=-"], wanted)
            # Even a success response requires private equality verification.
            observed = existing_resource(
                "secret" if wanted["kind"] == "Secret" else "configmap",
                wanted["metadata"]["name"],
            )
            if observed is None:
                raise ProvisionError("CREATE_UNCONFIRMED_PRIVATE_KEYS_RETAINED")
            require_equal(observed, wanted)
    print("NEW_API_APP_RESOURCES_VERIFIED; fixed keys retained; workloads unchanged")


def main():
    arguments = sys.argv[1:]
    credentials_file = None
    if arguments == ["--apply"]:
        pass
    elif len(arguments) == 3 and arguments[:2] == ["--apply", "--credentials-file"]:
        credentials_file = arguments[2]
    else:
        print("Requires reviewed --apply [--credentials-file ABSOLUTE_ENV_PATH]; creates only New API app Secret and CA", file=sys.stderr)
        return 1
    os.umask(0o077)
    try:
        provision(credentials_file)
    except ProvisionError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, subprocess.SubprocessError):
        print("PROVISION_UNCONFIRMED; private keys retained; reconcile privately before retry", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
