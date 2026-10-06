/// <reference types="vite/client" />

declare global {
  interface Window {
    __OBJBOX_AUTH_MODE__?: string
  }
}

export {}
