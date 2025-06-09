#!/bin/bash

chown -R $PUID:$PGID /opt/premiumizearrd/
sed -i "" "s/###USER###/$PUID/g" /etc/systemd/system/premiumizearrd.service
sed -i "" "s/###GROUP###/$PGID/g" /etc/systemd/system/premiumizearrd.service
systemctl enable premiumizearrd.service
systemctl daemon-reload
systemctl start premiumizearrd.service
