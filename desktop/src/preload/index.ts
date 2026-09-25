// Bridge for the control window only; the panel window gets no preload.
import { contextBridge, ipcRenderer } from 'electron'
import type { Api, AppState } from '../shared/types'

const api: Api = {
  onState: (cb) => void ipcRenderer.on('state', (_e, s: AppState) => cb(s)),
  onLog: (cb) => void ipcRenderer.on('log', (_e, l: string) => cb(l)),
  start: () => ipcRenderer.invoke('start'),
  stop: () => ipcRenderer.invoke('stop'),
  onOpenPanel: (cb) => void ipcRenderer.on('openPanel', () => cb()),
  setPanelOpen: (open) => ipcRenderer.invoke('setPanelOpen', open),
  copy: (id) => ipcRenderer.invoke('copy', id),
  setSysProxy: (on) => ipcRenderer.invoke('setSysProxy', on),
  setService: (name) => ipcRenderer.invoke('setService', name),
}
contextBridge.exposeInMainWorld('vm', api)
