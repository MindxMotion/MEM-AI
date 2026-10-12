"""Create only the dedicated MySQL Secret; never starts a database or rotates keys."""

import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import tempfile

spec = importlib.util.spec_from_file_location("application_credentials", Path(__file__).with_name("application-secret.py"))
app = importlib.util.module_from_spec(spec)
spec.loader.exec_module(app)

DIRECTORY = app.DIRECTORY
HOST = "new-api-mysql.new-api.svc.cluster.local"
SECRET = "new-api-mysql-auth"
BUNDLE = "mysql-package.json"
MATERIAL_KEYS = {
    "mysql-root-password", "mysql-app-password", "mysql-ca.pem",
    "mysql-ca-key.pem", "mysql-server.pem", "mysql-server-key.pem", "mysql.env",
}


def openssl(arguments):
    result = subprocess.run(
        ["openssl", *arguments], text=True, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, timeout=60, check=False,
    )
    if result.returncode:
        raise app.ProvisionError("MYSQL_TLS_VALIDATION_OR_GENERATION_FAILED_NO_ROTATION")
    return result.stdout.strip()


def generate_material(directory):
    # All subprocess arguments are public certificate metadata or protected paths,
    # never private key text, passwords, DSNs, SQL or Secret contents.
    ca_key, ca, server_key = (directory / name for name in ("ca-key.pem", "ca.pem", "server-key.pem"))
    csr, server, extensions = (directory / name for name in ("server.csr", "server.pem", "server.ext"))
    openssl(["genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:3072", "-out", str(ca_key)])
    openssl(["req", "-x509", "-new", "-sha256", "-key", str(ca_key), "-days", "3650",
             "-subj", "/CN=New API private MySQL CA", "-addext", "basicConstraints=critical,CA:TRUE,pathlen:0",
             "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-out", str(ca)])
    openssl(["genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:3072", "-out", str(server_key)])
    openssl(["req", "-new", "-sha256", "-key", str(server_key), "-subj", f"/CN={HOST}", "-out", str(csr)])
    extensions.write_text(
        "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
        f"extendedKeyUsage=serverAuth\nsubjectAltName=DNS:{HOST}\n", encoding="utf-8",
    )
    openssl(["x509", "-req", "-sha256", "-in", str(csr), "-CA", str(ca),
             "-CAkey", str(ca_key), "-set_serial", "0x" + secrets.token_hex(16),
             "-days", "365", "-extfile", str(extensions), "-out", str(server)])
    root_password, password = secrets.token_hex(32), secrets.token_hex(32)
    env = {
        "MYSQL_HOST": HOST, "MYSQL_PORT": "3306", "MYSQL_DATABASE": "new_api",
        "MYSQL_USER": "new_api_app", "MYSQL_PASSWORD": password,
        "MYSQL_SSL_MODE": "VERIFY_IDENTITY",
        "SQL_DSN": f"new_api_app:{password}@tcp({HOST}:3306)/new_api?tls=true&parseTime=true&charset=utf8mb4",
        "SSL_CERT_FILE": (DIRECTORY / "mysql-ca.pem").as_posix(),
    }
    material = {
        "mysql-root-password": root_password, "mysql-app-password": password,
        "mysql-ca.pem": ca.read_text(), "mysql-ca-key.pem": ca_key.read_text(),
        "mysql-server.pem": server.read_text(), "mysql-server-key.pem": server_key.read_text(),
        "mysql.env": "".join(f"{key}={value}\n" for key, value in env.items()),
    }
    for path in directory.iterdir():
        os.chmod(path, 0o600)
    return material


def validate_material(material, directory):
    if not isinstance(material, dict) or material.keys() != MATERIAL_KEYS:
        raise app.ProvisionError("MYSQL_PRIVATE_PACKAGE_KEYS_INVALID")
    if any(not isinstance(value, str) or not value for value in material.values()):
        raise app.ProvisionError("MYSQL_PRIVATE_PACKAGE_VALUES_INVALID")
    password, root_password = material["mysql-app-password"], material["mysql-root-password"]
    if (not re.fullmatch(r"[a-f0-9]{64}", password)
            or not re.fullmatch(r"[a-f0-9]{64}", root_password) or password == root_password):
        raise app.ProvisionError("MYSQL_FIXED_PASSWORDS_INVALID")
    env = app.parse_literal(material["mysql.env"], app.MYSQL_KEYS)
    expected = {
        "MYSQL_HOST": HOST, "MYSQL_PORT": "3306", "MYSQL_DATABASE": "new_api",
        "MYSQL_USER": "new_api_app", "MYSQL_PASSWORD": password, "MYSQL_SSL_MODE": "VERIFY_IDENTITY",
        "SQL_DSN": f"new_api_app:{password}@tcp({HOST}:3306)/new_api?tls=true&parseTime=true&charset=utf8mb4",
        "SSL_CERT_FILE": (DIRECTORY / "mysql-ca.pem").as_posix(),
    }
    if env != expected:
        raise app.ProvisionError("MYSQL_LITERAL_CONFIG_DRIFT")
    # Validate persisted cryptographic material, not just its PEM labels.
    for name in ("mysql-ca.pem", "mysql-ca-key.pem", "mysql-server.pem", "mysql-server-key.pem"):
        path = directory / name
        path.write_text(material[name], encoding="utf-8")
        os.chmod(path, 0o600)
    ca, server = str(directory / "mysql-ca.pem"), str(directory / "mysql-server.pem")
    openssl(["verify", "-CAfile", ca, "-verify_hostname", HOST, "-purpose", "sslserver", server])
    for cert, key in ((ca, "mysql-ca-key.pem"), (server, "mysql-server-key.pem")):
        openssl(["x509", "-in", cert, "-checkend", str(30 * 86400), "-noout"])
        cert_public = openssl(["x509", "-in", cert, "-pubkey", "-noout"])
        key_public = openssl(["pkey", "-in", str(directory / key), "-pubout"])
        if cert_public != key_public:
            raise app.ProvisionError("MYSQL_CERTIFICATE_PRIVATE_KEY_MISMATCH")


def provision():
    if os.geteuid() != 0:
        raise app.ProvisionError("ROOT_REQUIRED")
    if not DIRECTORY.exists() and not DIRECTORY.is_symlink():
        DIRECTORY.mkdir(mode=0o700)
    info = DIRECTORY.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700):
        raise app.ProvisionError("PROTECTED_DIRECTORY_INVALID")
    if app.kubectl(["config", "current-context"]) != app.CONTEXT:
        raise app.ProvisionError("CONTEXT_INVALID")
    app.kubectl(["get", "namespace", app.NAMESPACE, "--output=name"])
    existing = app.existing_resource("secret", SECRET)
    bundle_path = DIRECTORY / BUNDLE
    with tempfile.TemporaryDirectory(prefix=".mysql-material.", dir=DIRECTORY) as temporary:
        temporary_path = Path(temporary)
        os.chmod(temporary_path, 0o700)
        if bundle_path.exists() or bundle_path.is_symlink():
            material = json.loads(app.protected_read(bundle_path))
        else:
            pending = app.pending_publication(bundle_path)
            # Never adopt an existing identity without its retained private package.
            if pending is None and (existing is not None or any((DIRECTORY / name).exists() or (DIRECTORY / name).is_symlink() for name in MATERIAL_KEYS)):
                raise app.ProvisionError("MYSQL_IDENTITY_OR_PARTIAL_PACKAGE_EXISTS_NO_ROTATION")
            material = json.loads(pending) if pending is not None else generate_material(temporary_path)
            validate_material(material, temporary_path)
            # Atomic authoritative package precedes every public export and API write.
            # Interrupted exports are recoverable from these exact fixed values.
            app.atomic_publish(bundle_path, pending if pending is not None else json.dumps(material))
        validate_material(material, temporary_path)
        expected = app.resource("Secret", SECRET, {
            "root-password": material["mysql-root-password"], "app-password": material["mysql-app-password"],
            "ca.pem": material["mysql-ca.pem"], "server.pem": material["mysql-server.pem"],
            "server-key.pem": material["mysql-server-key.pem"],
        })
        app.require_equal(existing, expected)
        # Preflight every export before writing any missing ones.
        for name, content in material.items():
            path = DIRECTORY / name
            if (path.exists() or path.is_symlink()) and app.protected_read(path) != content:
                raise app.ProvisionError("MYSQL_PRIVATE_PACKAGE_DRIFT_NO_OVERWRITE")
        for name, content in material.items():
            app.atomic_publish(DIRECTORY / name, content)
        if existing is None:
            app.kubectl(["create", "--filename=-"], expected)
            observed = app.existing_resource("secret", SECRET)
            if observed is None:
                raise app.ProvisionError("MYSQL_SECRET_CREATE_UNCONFIRMED_PRIVATE_PACKAGE_RETAINED")
            app.require_equal(observed, expected)
    print("NEW_API_MYSQL_SECRET_VERIFIED; fixed material retained; workloads unchanged")


def main():
    if sys.argv[1:] != ["--apply"]:
        print("Requires reviewed --apply; creates only dedicated New API MySQL credentials", file=sys.stderr)
        return 1
    os.umask(0o077)
    try:
        provision()
    except app.ProvisionError as error:
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, subprocess.SubprocessError):
        print("MYSQL_PROVISION_UNCONFIRMED; private material retained; reconcile without rotation", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
