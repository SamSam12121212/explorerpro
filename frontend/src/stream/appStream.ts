import { WebSocket as PartySocketWebSocket } from "partysocket";
import { buildStreamWebSocketUrl } from "../api";
import type { ThreadStreamMessage } from "../types";

type MessageListener = (payload: ThreadStreamMessage) => void;
type Status = "online" | "degraded";
type StatusListener = (status: Status) => void;

class AppStreamManager {
  private socket: PartySocketWebSocket | null = null;
  private closingSocket: PartySocketWebSocket | null = null;
  private readonly messageListeners = new Set<MessageListener>();
  private readonly statusListeners = new Set<StatusListener>();
  private readonly pendingMessages: ThreadStreamMessage[] = [];
  private currentStatus: Status | null = null;

  subscribe(listener: MessageListener) {
    this.messageListeners.add(listener);
    if (this.pendingMessages.length > 0) {
      const backlog = this.pendingMessages.splice(0, this.pendingMessages.length);
      for (const payload of backlog) {
        listener(payload);
      }
    }
    return () => {
      this.messageListeners.delete(listener);
    };
  }

  subscribeStatus(listener: StatusListener) {
    this.statusListeners.add(listener);
    if (this.currentStatus) {
      listener(this.currentStatus);
    }
    return () => {
      this.statusListeners.delete(listener);
    };
  }

  connect() {
    if (this.socket) {
      return;
    }
    this.socket = this.createSocket();
  }

  disconnect() {
    if (!this.socket) {
      return;
    }

    const socket = this.socket;
    this.socket = null;
    this.closingSocket = socket;
    socket.close();
  }

  private notifyStatus(status: Status) {
    this.currentStatus = status;
    for (const listener of this.statusListeners) {
      listener(status);
    }
  }

  private notifyMessage(payload: ThreadStreamMessage) {
    if (this.messageListeners.size === 0) {
      this.pendingMessages.push(payload);
      if (this.pendingMessages.length > 256) {
        this.pendingMessages.splice(0, this.pendingMessages.length - 256);
      }
      return;
    }

    for (const listener of this.messageListeners) {
      listener(payload);
    }
  }

  private readonly handleOpen = () => {
    this.notifyStatus("online");
  };

  private readonly handleMessage = (event: MessageEvent) => {
    try {
      const payload = JSON.parse(event.data as string) as ThreadStreamMessage;
      this.notifyMessage(payload);
    } catch {
      /* swallow invalid websocket payloads */
    }
  };

  private readonly handleError = () => {
    this.notifyStatus("degraded");
  };

  private readonly handleClose = (event: CloseEvent) => {
    if (this.closingSocket === event.target) {
      this.closingSocket = null;
      return;
    }

    if (this.socket !== event.target) {
      return;
    }

    this.notifyStatus("degraded");
  };

  private createSocket() {
    const socket = new PartySocketWebSocket(
      buildStreamWebSocketUrl("/connect"),
      [],
      {
        connectionTimeout: 10000,
        minReconnectionDelay: 1000,
        maxReconnectionDelay: 10000,
      },
    );

    socket.onopen = this.handleOpen;
    socket.onmessage = this.handleMessage;
    socket.onerror = this.handleError;
    socket.onclose = this.handleClose;

    return socket;
  }
}

export const appStreamManager = new AppStreamManager();
