import axios, { AxiosError, type AxiosRequestConfig } from "axios";

import webConfig from "@/constants/common-env";
import { requestReauth } from "@/store/auth";

type RequestConfig = AxiosRequestConfig & {
  redirectOnUnauthorized?: boolean;
};

type ErrorPayload = {
  detail?: { error?: string; code?: string; message?: string };
  error?: string | { message?: string; code?: string };
  message?: string;
  code?: string;
};

export class ApiError extends Error {
  code?: string;
  status?: number;

  constructor(message: string, options: { code?: string; status?: number } = {}) {
    super(message);
    this.name = "ApiError";
    this.code = options.code;
    this.status = options.status;
  }
}

// Multi-tenant auth model (docs/multi-tenant-redesign.md §4.6): the session
// lives in an HttpOnly cookie, so every request must send credentials and we
// never attach a bearer token. A 401 means the session expired/was never
// established; ask the mother system to re-issue an entry ticket.
const request = axios.create({
  baseURL: webConfig.apiUrl.replace(/\/$/, ""),
  withCredentials: true,
});

request.interceptors.response.use(
  (response) => response,
  async (error: AxiosError<ErrorPayload>) => {
    const status = error.response?.status;
    const shouldReauth =
      (error.config as RequestConfig | undefined)?.redirectOnUnauthorized !== false;
    if (status === 401 && shouldReauth && typeof window !== "undefined") {
      requestReauth();
    }

    const payload = error.response?.data;
    const nestedError =
      payload && typeof payload.error === "object" && payload.error
        ? (payload.error as { message?: string; code?: string })
        : null;
    const code = payload?.detail?.code || nestedError?.code || payload?.code;
    const message =
      payload?.detail?.error ||
      payload?.detail?.message ||
      nestedError?.message ||
      (typeof payload?.error === "string" ? payload.error : "") ||
      payload?.message ||
      error.message ||
      `请求失败 (${status || 500})`;
    return Promise.reject(new ApiError(message, { code, status }));
  },
);

type RequestOptions = {
  method?: string;
  body?: unknown;
  headers?: Record<string, string>;
  timeoutMs?: number;
  redirectOnUnauthorized?: boolean;
};

export async function httpRequest<T>(path: string, options: RequestOptions = {}) {
  const {
    method = "GET",
    body,
    headers,
    timeoutMs,
    redirectOnUnauthorized = true,
  } = options;
  const config: RequestConfig = {
    url: path,
    method,
    data: body,
    headers,
    timeout: timeoutMs,
    redirectOnUnauthorized,
  };
  const response = await request.request<T>(config);
  return response.data;
}
