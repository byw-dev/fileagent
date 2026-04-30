import apiClient from '../services/api'

/**
 * Download a single file by obtaining its presigned URL and triggering a browser download.
 * @param fileId - UUID of the file entry.
 * @param fileName - Suggested filename for the download.
 */
export async function downloadSingle(fileId: string, fileName: string): Promise<void> {
  const response = await apiClient.get<{ url: string }>(
    `/api/v1/files/${fileId}/download-url`
  )
  const { url } = response.data
  const a = document.createElement('a')
  a.href = url
  a.download = fileName
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
}

/**
 * Obtain a presigned download URL for a single file without triggering the browser download.
 * @param fileId - UUID of the file entry.
 * @returns The presigned download URL string.
 */
export async function getDownloadUrl(fileId: string): Promise<string> {
  const response = await apiClient.get<{ url: string }>(
    `/api/v1/files/${fileId}/download-url`
  )
  return response.data.url
}
