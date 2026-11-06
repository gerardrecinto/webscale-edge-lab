package dev.pushbutton;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import org.junit.jupiter.api.Test;

class AppTest {
    private final HttpClient client = HttpClient.newHttpClient();

    private HttpResponse<String> get(App app, String path) throws Exception {
        return client.send(HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + app.port() + path)).build(),
                HttpResponse.BodyHandlers.ofString());
    }

    @Test
    void answersWithItsIdentityAndSecurityHeaders() throws Exception {
        App app = new App("shop-1", "v7", 0);
        app.start();
        try {
            HttpResponse<String> r = get(app, "/");
            assertEquals(200, r.statusCode());
            assertEquals("shop-1", r.body());
            assertEquals("v7", r.headers().firstValue("X-Version").orElse(""));
            assertEquals("nosniff", r.headers().firstValue("X-Content-Type-Options").orElse(""));
            assertTrue(r.headers().firstValue("Content-Security-Policy").orElse("").contains("frame-ancestors 'none'"));
            assertTrue(r.headers().firstValue("Strict-Transport-Security").isPresent());
            assertEquals(200, get(app, "/healthz").statusCode());
            assertEquals("0", get(app, "/debug/inflight").body());
        } finally {
            app.drain(1000);
        }
    }

    @Test
    void drainingReportsNotReadyThenStops() throws Exception {
        App app = new App("shop-2", "v7", 0);
        app.start();
        assertTrue(app.ready());
        assertTrue(app.drain(1000));
        assertTrue(!app.ready());
    }
}
