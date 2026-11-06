package dev.pushbutton;

import com.sun.net.httpserver.Headers;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.Executors;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * A small HTTP service that behaves the way a deploy controller needs:
 * it says when it is ready, counts the requests it is working on, and on
 * shutdown stops reporting ready, finishes what it is doing, then exits.
 */
public final class App {
    private final String id;
    private final String version;
    private final HttpServer server;
    private final ExecutorService pool;
    private final AtomicInteger inflight = new AtomicInteger();
    private volatile boolean ready = true;

    public App(String id, String version, int port) throws IOException {
        this.id = id;
        this.version = version;
        this.server = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 128);
        this.pool = Executors.newFixedThreadPool(32);
        this.server.setExecutor(pool);
        this.server.createContext("/healthz", this::health);
        this.server.createContext("/debug/inflight", this::inflightGauge);
        this.server.createContext("/", this::handle);
    }

    public void start() {
        server.start();
    }

    public int port() {
        return server.getAddress().getPort();
    }

    public int inflight() {
        return inflight.get();
    }

    public boolean ready() {
        return ready;
    }

    /**
     * Graceful stop: report not ready so the load balancer stops sending new
     * requests, wait for the ones already running, then close the listener.
     *
     * @return true if everything finished inside the timeout
     */
    public boolean drain(long timeoutMillis) throws InterruptedException {
        ready = false;
        long deadline = System.nanoTime() + timeoutMillis * 1_000_000L;
        while (inflight.get() > 0 && System.nanoTime() < deadline) {
            Thread.sleep(10);
        }
        boolean clean = inflight.get() == 0;
        server.stop(0);
        pool.shutdown();
        return clean;
    }

    private void health(HttpExchange ex) throws IOException {
        if (ready) {
            send(ex, 200, "ok");
        } else {
            send(ex, 503, "draining");
        }
    }

    private void inflightGauge(HttpExchange ex) throws IOException {
        send(ex, 200, Integer.toString(inflight.get()));
    }

    private void handle(HttpExchange ex) throws IOException {
        inflight.incrementAndGet();
        try {
            send(ex, 200, id);
        } finally {
            inflight.decrementAndGet();
        }
    }

    private void send(HttpExchange ex, int status, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        Headers h = ex.getResponseHeaders();
        h.set("Content-Type", "text/plain; charset=utf-8");
        h.set("X-Backend", id);
        h.set("X-Version", version);
        securityHeaders(h);
        ex.sendResponseHeaders(status, bytes.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(bytes);
        }
    }

    /** The headers the OWASP Secure Headers Project recommends for a service nobody should frame or sniff. */
    static void securityHeaders(Headers h) {
        h.set("Strict-Transport-Security", "max-age=31536000; includeSubDomains");
        h.set("X-Content-Type-Options", "nosniff");
        h.set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'");
        h.set("X-Frame-Options", "DENY");
        h.set("Referrer-Policy", "no-referrer");
        h.set("Permissions-Policy", "geolocation=(), camera=(), microphone=()");
        h.set("Cache-Control", "no-store");
    }
}
