package com.example.demo;

import java.net.InetAddress;
import java.time.Instant;
import java.util.Map;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
public class HealthController {

    // 平台健康探针路径：/health。
    @GetMapping("/health")
    public Map<String, String> health() {
        return Map.of("status", "ok");
    }

    @GetMapping("/")
    public Map<String, String> root() {
        return Map.of(
                "service", "web-springboot",
                "host", InetAddress.getLoopbackAddress().getHostName(),
                "time", Instant.now().toString(),
                "message", "Hello from Yummy PaaS");
    }
}
