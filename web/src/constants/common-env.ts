// apiUrl is the base every API/SSE/image-file request is built on.
// - Production: the app is embedded at the mother system's `/image-studio/*`
//   sub-path behind a same-origin reverse proxy, so all browser-visible URLs
//   carry that prefix (the proxy strips it before the backend sees the request).
// - Development: talk directly to the local backend. Use `localhost` (not
//   127.0.0.1) so a Windows-host browser can reach a backend running inside
//   WSL2 — Windows forwards localhost into WSL, but 127.0.0.1 resolves to the
//   Windows loopback and never enters WSL.
const webConfig = {
    apiUrl: import.meta.env.DEV ? 'http://localhost:7000' : '/image-studio',
};

export default webConfig;
