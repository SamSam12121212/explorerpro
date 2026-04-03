import type { HealthResponse, UploadedImage } from "./types";

async function readJson<T>(response: Response) {
  const text = await response.text();
  const data = text ? (JSON.parse(text) as T) : ({} as T);

  if (!response.ok) {
    const errorMessage =
      typeof data === "object" && data && "error" in data
        ? ((data as { error?: { message?: string } }).error?.message ??
          response.statusText)
        : `${response.status.toString()} ${response.statusText}`;
    throw new Error(errorMessage);
  }

  return data;
}

export async function apiGet<T>(path: string) {
  const response = await fetch(path);
  return readJson<T>(response);
}

export async function apiPost<T>(path: string, body: unknown) {
  const response = await fetch(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  return readJson<T>(response);
}

export async function uploadImage(file: File) {
  const formData = new FormData();
  formData.append("file", file, file.name || "image");

  const response = await fetch("/images", {
    method: "POST",
    body: formData,
  });

  const payload = await readJson<{
    image: Omit<UploadedImage, "preview_url">;
  }>(response);
  return payload.image;
}

export async function checkHealthApi(): Promise<"online" | "degraded"> {
  const payload = await apiGet<HealthResponse>("/healthz");
  return payload.status === "ok" ? "online" : "degraded";
}
