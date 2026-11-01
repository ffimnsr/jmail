#!/usr/bin/env python3

import smtplib
import ssl

smtp_server = "<ip-of-mail-server>"
smtp_port = 11587
smtp_auth_user = "jmail"
smtp_auth_password = "mysecretpassword"

# jmail serves a self-signed certificate until you replace it, so verification is
# skipped here. Point smtp_server at the certificate's JMAIL_ORIGIN and drop the
# next two lines once you use a certificate from a public CA.
context = ssl.create_default_context()
context.check_hostname = False
context.verify_mode = ssl.CERT_NONE

# Try to log in to server
try:
    with smtplib.SMTP(smtp_server, smtp_port) as server:
        server.starttls(context=context)
        server.login(smtp_auth_user, smtp_auth_password)
        print("Authentication succeeded")
except (smtplib.SMTPException, OSError) as exc:
    # Print any error messages to stdout
    print(exc)
