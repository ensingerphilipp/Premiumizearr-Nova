#!/bin/bash

# Set default UID and GID if not provided
PUID=${PUID:-1000}
PGID=${PGID:-1000}

# Set permissions
chown -R "$PUID:$PGID" /opt/premiumizearrd/

# Replace placeholders in systemd service file
sed -i "" "s/###USER###/$PUID/g" /etc/systemd/system/premiumizearrd.service
sed -i "" "s/###GROUP###/$PGID/g" /etc/systemd/system/premiumizearrd.service

# Enable and start the service
systemctl enable premiumizearrd.service
systemctl daemon-reload
systemctl start premiumizearrd.service
