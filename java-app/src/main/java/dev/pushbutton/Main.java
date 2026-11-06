package dev.pushbutton;

public final class Main {
    private Main() {
    }

    public static void main(String[] args) throws Exception {
        int port = 8080;
        String id = "shop-api";
        String version = System.getenv().getOrDefault("APP_VERSION", "dev");
        long drainMillis = 10_000;
        for (int i = 0; i + 1 < args.length; i += 2) {
            switch (args[i]) {
                case "--port" -> port = Integer.parseInt(args[i + 1]);
                case "--id" -> id = args[i + 1];
                case "--version" -> version = args[i + 1];
                case "--drain-ms" -> drainMillis = Long.parseLong(args[i + 1]);
                default -> throw new IllegalArgumentException("unknown flag " + args[i]);
            }
        }
        final App app = new App(id, version, port);
        final long drain = drainMillis;
        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            try {
                boolean clean = app.drain(drain);
                System.err.println("drained, clean=" + clean);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }));
        app.start();
        System.out.println("listening on 127.0.0.1:" + app.port() + " as " + id + " " + version);
    }
}
