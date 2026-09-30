import http.client
from contextlib import closing
import json
import logging
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest

import server


class ActivityWatchTlsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cert_tmp = tempfile.TemporaryDirectory()
        directory = Path(cls.cert_tmp.name)
        cls.cert = directory / 'test-cert.pem'
        cls.key = directory / 'test-key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-keyout', str(cls.key), '-out', str(cls.cert), '-days', '1',
                        '-subj', '/CN=localhost'], check=True, capture_output=True, timeout=15)
        cls.key.chmod(0o600)
        cls.old_log_level = server.LOGGER.level
        server.LOGGER.setLevel(logging.CRITICAL)

    @classmethod
    def tearDownClass(cls):
        server.LOGGER.setLevel(cls.old_log_level)
        cls.cert_tmp.cleanup()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db = server.Database(Path(self.tmp.name) / 'synthetic.sqlite3')
        self.db.initialize()
        self.config = server.Config(bind_host='127.0.0.1', port=0, token='x' * 40,
                 db_path=self.db.path, cert_path=self.cert, key_path=self.key,
                 max_body_bytes=16 * 1024 * 1024)
        self.http = server.SyncHttpServer(('127.0.0.1', 0), self.config, self.db)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(self.cert, self.key)
        self.http.socket = context.wrap_socket(self.http.socket, server_side=True,
                                               do_handshake_on_connect=False)
        self.port = self.http.server_address[1]
        self.thread = threading.Thread(target=self.http.serve_forever,
                                       kwargs={'poll_interval': 0.02}, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.http.shutdown()
        self.http.server_close()
        self.thread.join(2)
        self.tmp.cleanup()

    def connection(self, timeout=2):
        return closing(http.client.HTTPSConnection('127.0.0.1', self.port, timeout=timeout,
                                           context=ssl._create_unverified_context()))

    def payload(self):
        return {'schema_version': 1, 'batch_id': 'synthetic-tls-test',
                'sent_at': '2026-09-30T00:00:00Z',
                'device': {'id': 'synthetic-device', 'hostname': 'synthetic-host'},
                'buckets': [{'id': 'synthetic-bucket', 'events': [
                    {'id': 1, 'timestamp': '2026-09-30T00:00:00Z',
                     'duration': 1, 'data': {'padding': ''}}]}]}

    def body(self, desired_size):
        payload = self.payload()
        compact = lambda: json.dumps(payload, separators=(',', ':')).encode()
        base = compact()
        payload['buckets'][0]['events'][0]['data']['padding'] = 'x' * (desired_size - len(base))
        body = compact()
        self.assertEqual(len(body), desired_size)
        return body

    def test_stalled_tls_client_does_not_block_second_https_health(self):
        stalled = socket.create_connection(('127.0.0.1', self.port), timeout=2)
        try:
            time.sleep(0.05)
            started = time.monotonic()
            with self.connection() as connection:
                connection.request('GET', '/health')
                response = connection.getresponse()
                result = json.loads(response.read())
            self.assertEqual(response.status, 200)
            self.assertEqual(result['service'], 'activitywatch-sync')
            self.assertLess(time.monotonic() - started, 1.5)
        finally:
            stalled.close()

    def test_stalled_tls_connection_is_closed_at_handshake_deadline(self):
        self.http.tls_handshake_timeout_seconds = 0.15
        with socket.create_connection(('127.0.0.1', self.port), timeout=1) as stalled:
            stalled.settimeout(1)
            self.assertEqual(stalled.recv(1), b'')

    def test_original_listener_handshake_mode_reproduces_global_block(self):
        self.http.socket.do_handshake_on_connect = True
        stalled = socket.create_connection(('127.0.0.1', self.port), timeout=2)
        try:
            time.sleep(0.05)
            with self.connection(timeout=0.2) as connection:
                with self.assertRaises(TimeoutError):
                    connection.request('GET', '/health')
        finally:
            stalled.close()

    def test_full_sixteen_mib_authenticated_upload_succeeds(self):
        body = self.body(self.config.max_body_bytes)
        with self.connection(timeout=10) as connection:
            connection.request('POST', '/v1/sync', body, headers={
                 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + self.config.token})
            response = connection.getresponse()
            result = json.loads(response.read())
        self.assertEqual(response.status, 200)
        self.assertEqual(result['status'], 'ok')
        self.assertEqual(result['batch_id'], 'synthetic-tls-test')
        self.assertEqual(len(self.db.list_events(device_id='synthetic-device', bucket_id=None,
                                                start=None, end=None, limit=5)), 1)

    def test_slow_normal_upload_outlasts_handshake_deadline(self):
        self.http.tls_handshake_timeout_seconds = 0.15
        self.http.request_idle_timeout_seconds = 0.25
        body = self.body(65536)
        with self.connection() as connection:
            connection.putrequest('POST', '/v1/sync')
            connection.putheader('Content-Type', 'application/json')
            connection.putheader('Content-Length', str(len(body)))
            connection.putheader('Authorization', 'Bearer ' + self.config.token)
            connection.endheaders()
            started = time.monotonic()
            for offset in range(0, len(body), 8192):
                connection.send(body[offset:offset + 8192])
                time.sleep(0.04)
            response = connection.getresponse()
            result = json.loads(response.read())
        self.assertGreater(time.monotonic() - started, self.http.tls_handshake_timeout_seconds)
        self.assertEqual(response.status, 200)
        self.assertEqual(result['status'], 'ok')

    def test_authentication_and_body_limit_unchanged(self):
        with self.connection() as connection:
            connection.request('GET', '/v1/status')
            response = connection.getresponse()
            self.assertEqual(response.status, 401)
            response.read()
        with self.connection() as connection:
            connection.putrequest('POST', '/v1/sync')
            connection.putheader('Content-Type', 'application/json')
            connection.putheader('Content-Length', str(self.config.max_body_bytes + 1))
            connection.putheader('Authorization', 'Bearer ' + self.config.token)
            connection.endheaders()
            response = connection.getresponse()
            self.assertEqual(response.status, 413)
            response.read()


if __name__ == '__main__':
    unittest.main()
