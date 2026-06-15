// apiUrl is the base every API/SSE/image-file request is built on.
// - Production: the app is embedded at the mother system's `/image-studio/*`
//   sub-path behind a same-origin reverse proxy, so all browser-visible URLs
//   carry that prefix (the proxy strips it before the backend sees the request).
// - Development: talk directly to the local backend.
const webConfig = {
    apiUrl: import.meta.env.DEV ? 'http://127.0.0.1:7000' : '/image-studio',
};

export default webConfig;
