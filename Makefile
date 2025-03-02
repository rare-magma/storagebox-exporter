.PHONY: install
install:
	@mkdir --parents $${HOME}/.local/bin \
	&& mkdir --parents $${HOME}/.config/systemd/user \
	&& cp storagebox_exporter $${HOME}/.local/bin/ \
	&& chmod +x $${HOME}/.local/bin/storagebox_exporter \
	&& cp --no-clobber storagebox_exporter.json $${HOME}/.config/storagebox_exporter.json \
	&& chmod 400 $${HOME}/.config/storagebox_exporter.json \
	&& cp storagebox-exporter.timer $${HOME}/.config/systemd/user/ \
	&& cp storagebox-exporter.service $${HOME}/.config/systemd/user/ \
	&& systemctl --user enable --now storagebox-exporter.timer

.PHONY: uninstall
uninstall:
	@rm -f $${HOME}/.local/bin/storagebox_exporter \
	&& rm -f $${HOME}/.config/storagebox_exporter.json \
	&& systemctl --user disable --now storagebox-exporter.timer \
	&& rm -f $${HOME}/.config/.config/systemd/user/storagebox-exporter.timer \
	&& rm -f $${HOME}/.config/systemd/user/storagebox-exporter.service

.PHONY: build
build:
	@go build -ldflags="-s -w" -o storagebox_exporter main.go