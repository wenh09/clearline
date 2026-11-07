# Read-only Hysteria 2 pairing helper, bundled with the client.
# No credentials or private keys are printed. Output is encrypted to this Mac.
import base64
import json
import pathlib
import re
import shlex
import subprocess
import urllib.parse


def read_config():
    paths = [
        '/etc/clearline/server.json', '/etc/clearline/hysteria.yaml',
        '/etc/hysteria/config.yaml', '/etc/hysteria/config.json',
    ]
    for service in ['clearline-hysteria.service', 'hysteria-server.service', 'hysteria.service']:
        try:
            unit = subprocess.check_output(
                ['systemctl', 'show', service, '--property=ExecStart', '--value'],
                text=True, stderr=subprocess.DEVNULL, timeout=3)
            match = re.search(r'argv\[\]=(.*?);', unit)
            if match:
                args = shlex.split(match.group(1))
                for flag in ['-c', '--config']:
                    if flag in args and args.index(flag) + 1 < len(args):
                        paths.insert(0, args[args.index(flag) + 1])
        except Exception:
            pass
    for name in dict.fromkeys(paths):
        path = pathlib.Path(name)
        if not path.is_file() or path.stat().st_size > 128 * 1024:
            continue
        raw = path.read_text()
        try:
            doc = json.loads(raw)
        except ValueError:
            try:
                import yaml
                doc = yaml.safe_load(raw)
            except Exception:
                continue
        if isinstance(doc, dict) and 'auth' in doc and 'tls' in doc:
            return doc
    raise ValueError('CONFIG_NOT_FOUND')


def main(config):
    try:
        import ipaddress
        import os
        from cryptography import x509
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import padding
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM

        host = str(ipaddress.ip_address(config['host']))
        doc = read_config()
        auth = doc['auth']
        if auth.get('type') == 'password' and isinstance(auth.get('password'), str):
            password = auth['password']
        elif auth.get('type') == 'userpass' and isinstance(auth.get('userpass'), dict):
            users = auth['userpass']
            user = next(u for u in sorted(users) if isinstance(users[u], str) and users[u])
            password = user + ':' + users[user]
        else:
            raise ValueError('UNSUPPORTED_AUTH')
        if not password:
            raise ValueError('UNSUPPORTED_AUTH')
        cert_path = pathlib.Path(doc['tls']['cert'])
        if cert_path.stat().st_size > 32 * 1024:
            raise ValueError('INVALID_CERTIFICATE')
        certificate = cert_path.read_bytes()
        cert = x509.load_pem_x509_certificate(certificate)
        san = cert.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
        names = san.get_values_for_type(x509.DNSName)
        ips = san.get_values_for_type(x509.IPAddress)
        if names:
            sni = names[0]
        elif ipaddress.ip_address(host) in ips:
            sni = host
        else:
            raise ValueError('INVALID_CERTIFICATE')
        port = int(str(doc.get('listen', ':443')).rsplit(':', 1)[1])
        if not 1 <= port <= 65535:
            raise ValueError('INVALID_PORT')
        query = {'sni': sni, 'ca': base64.urlsafe_b64encode(certificate).decode().rstrip('=')}
        obfs = doc.get('obfs') or {}
        if obfs:
            if obfs.get('type') != 'salamander':
                raise ValueError('UNSUPPORTED_OBFS')
            query['obfs'] = 'salamander'
            query['obfs-password'] = obfs['salamander']['password']
        address = '[' + host + ']' if ':' in host else host
        link = ('hysteria2://' + urllib.parse.quote(password, safe='') + '@' + address
                + ':' + str(port) + '/?' + urllib.parse.urlencode(query))
        payload = {'challenge': config['challenge'], 'serverID': config['serverID'],
                   'profile': {'kind': 'hysteria2', 'name': config['name'], 'config': link}}
        raw = json.dumps(payload, ensure_ascii=False, separators=(',', ':')).encode()
        public = serialization.load_pem_public_key(base64.b64decode(config['publicKey']))
        key, nonce = os.urandom(32), os.urandom(12)
        data = AESGCM(key).encrypt(nonce, raw, b'clearline-existing-server-v1')
        wrapped = public.encrypt(key, padding.OAEP(
            mgf=padding.MGF1(algorithm=hashes.SHA256()), algorithm=hashes.SHA256(), label=None))
        envelope = {k: base64.b64encode(v).decode() for k, v in
                    [('key', wrapped), ('nonce', nonce), ('data', data)]}
        code = base64.urlsafe_b64encode(json.dumps(envelope, separators=(',', ':')).encode()).decode().rstrip('=')
        print('CLEARLINE1:' + code + ':END', flush=True)
    except ImportError:
        print('CLEARLINE_READ_ERROR:MISSING_CRYPTOGRAPHY', flush=True)
    except Exception as exc:
        # Never reveal YAML snippets, passwords, file contents or raw errors.
        allowed = {'CONFIG_NOT_FOUND', 'UNSUPPORTED_AUTH', 'INVALID_CERTIFICATE', 'INVALID_PORT', 'UNSUPPORTED_OBFS'}
        code = str(exc) if isinstance(exc, ValueError) and str(exc) in allowed else 'READ_FAILED'
        print('CLEARLINE_READ_ERROR:' + code, flush=True)
