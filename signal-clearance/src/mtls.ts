import fs from 'fs';
import https from 'https';
import tls from 'tls';

export function buildMTLSOptions(): https.ServerOptions {
  const caCert = fs.readFileSync('/certs/ca.crt');
  const cert = fs.readFileSync('/certs/signal-clearance.crt');
  const key = fs.readFileSync('/certs/signal-clearance.key');

  return {
    ca: caCert,
    cert,
    key,
    requestCert: true,
    rejectUnauthorized: true,
    minVersion: 'TLSv1.3' as tls.SecureVersion,
  };
}

export function buildUpstreamAgent(): https.Agent {
  const caCert = fs.readFileSync('/certs/ca.crt');
  const cert = fs.readFileSync('/certs/signal-clearance.crt');
  const key = fs.readFileSync('/certs/signal-clearance.key');

  return new https.Agent({
    ca: caCert,
    cert,
    key,
    minVersion: 'TLSv1.3' as tls.SecureVersion,
  });
}
