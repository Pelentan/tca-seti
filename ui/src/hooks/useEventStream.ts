import { useState, useEffect, useRef, useCallback } from 'react';

const GATEWAY = import.meta.env.VITE_GATEWAY_URL || '';
const MAX_EVENTS = 200;

export interface SETIEvent {
  id: string;
  timestamp: string;
  caller: string;
  callee: string;
  method: string;
  path: string;
  statusCode: number;
  latencyMs: number;
  protocol: string;
}

interface StreamState {
  events: SETIEvent[];
  connected: boolean;
  error: string | null;
  eventCount: number;
}

export function useEventStream(jwt: string | null) {
  const [state, setState] = useState<StreamState>({
    events: [],
    connected: false,
    error: null,
    eventCount: 0,
  });

  const esRef = useRef<EventSource | null>(null);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const connect = useCallback(() => {
    if (!jwt) return;
    if (esRef.current) {
      esRef.current.close();
    }

    // Gateway SSE endpoint — JWT in query param since EventSource can't set headers
    const url = `${GATEWAY}/events?token=${encodeURIComponent(jwt)}`;
    const es = new EventSource(url);
    esRef.current = es;

    es.addEventListener('connected', () => {
      setState(prev => ({ ...prev, connected: true, error: null }));
    });

    es.onmessage = (e) => {
      try {
        const event: SETIEvent = JSON.parse(e.data);
        setState(prev => ({
          ...prev,
          eventCount: prev.eventCount + 1,
          events: [event, ...prev.events].slice(0, MAX_EVENTS),
        }));
      } catch {
        // Malformed event — skip
      }
    };

    es.onerror = () => {
      setState(prev => ({ ...prev, connected: false, error: 'Stream disconnected' }));
      es.close();
      // Reconnect after 3 seconds
      reconnectTimer.current = setTimeout(connect, 3000);
    };
  }, [jwt]);

  useEffect(() => {
    connect();
    return () => {
      esRef.current?.close();
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
    };
  }, [connect]);

  const clearEvents = useCallback(() => {
    setState(prev => ({ ...prev, events: [], eventCount: 0 }));
  }, []);

  return { ...state, clearEvents };
}
