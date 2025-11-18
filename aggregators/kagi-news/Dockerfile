# Kagi News RSS Aggregator
# Production-ready Docker image with cron scheduler

FROM python:3.11-slim

# Install cron and other utilities
RUN apt-get update && apt-get install -y \
    cron \
    curl \
    procps \
    && rm -rf /var/lib/apt/lists/*

# Set working directory
WORKDIR /app

# Copy requirements first for better caching
COPY requirements.txt .

# Install Python dependencies (exclude dev/test deps in production)
RUN pip install --no-cache-dir \
    feedparser==6.0.11 \
    beautifulsoup4==4.12.3 \
    requests==2.31.0 \
    atproto==0.0.55 \
    pyyaml==6.0.1

# Copy application code
COPY src/ ./src/
COPY config.yaml ./

# Copy crontab file
COPY crontab /etc/cron.d/kagi-aggregator

# Give execution rights on the cron job and apply it
RUN chmod 0644 /etc/cron.d/kagi-aggregator && \
    crontab /etc/cron.d/kagi-aggregator

# Create log file to be able to run tail
RUN touch /var/log/cron.log

# Copy entrypoint script
COPY docker-entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Health check - verify cron is running
HEALTHCHECK --interval=60s --timeout=10s --start-period=10s --retries=3 \
    CMD pgrep cron || exit 1

# Run the entrypoint script
ENTRYPOINT ["docker-entrypoint.sh"]

# Default command: tail the cron log
CMD ["tail", "-f", "/var/log/cron.log"]
