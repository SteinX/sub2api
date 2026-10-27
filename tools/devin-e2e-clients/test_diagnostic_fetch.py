import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class FetchDiagnosticTests(unittest.TestCase):
    def test_redirects_cannot_reach_another_loopback_origin(self):
        # Both servers are local. No real destination or credential is used.
        preload = str(Path(__file__).with_name('diagnostic-fetch.cjs'))
        script = r'''
const http = require('node:http'); let escaped = 0;
const outside = http.createServer((req, res) => { escaped++; res.end('bad'); });
outside.listen(0, '127.0.0.1', () => {
 const inside = http.createServer((req, res) => {
  res.writeHead(Number(req.url.slice(1)), {location: `http://127.0.0.1:${outside.address().port}/outside`}); res.end();
 });
 inside.listen(0, '127.0.0.1', async () => {
  process.env.DEVIN_JOURNEY_FETCH_ORIGIN = `http://127.0.0.1:${inside.address().port}`;
  require(process.argv[1]); let rejected = 0;
  for (const code of [302, 307]) {
   try { await fetch(`${process.env.DEVIN_JOURNEY_FETCH_ORIGIN}/${code}`, {method:'POST', body:'synthetic'}); }
   catch { rejected++; }
  }
  inside.close(); outside.close(); console.log(JSON.stringify({escaped, rejected}));
 });
});'''
        with tempfile.TemporaryDirectory() as temporary:
            env = {**os.environ, 'DEVIN_E2E_MODE': 'mock', 'DEVIN_JOURNEY_FETCH_TRACE': str(Path(temporary)/'trace')}
            result = subprocess.run(['node', '-e', script, preload], env=env, capture_output=True, timeout=20, check=True)
        self.assertEqual({'escaped': 0, 'rejected': 2}, json.loads(result.stdout))


if __name__ == '__main__':
    unittest.main()
