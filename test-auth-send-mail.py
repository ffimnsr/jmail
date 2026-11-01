#!/usr/bin/env python3

import os
import smtplib
import ssl
from email.mime.text import MIMEText

smtp_server = os.environ.get("JMAIL_TEST_SERVER", "<ip-of-mail-server>")
smtp_port = int(os.environ.get("JMAIL_TEST_PORT", "11587"))
smtp_auth_user = os.environ.get("JMAIL_TEST_USER", "jmail")
smtp_auth_password = os.environ.get("JMAIL_TEST_PASSWORD", "mysecretpassword")

sender = os.environ.get("JMAIL_TEST_SENDER", "noreply@<your-domain>")
receiver = os.environ.get("JMAIL_TEST_RECEIVER", "<your-email-address>")
email_body = "This is a test email from Jmail."

# jmail serves a self-signed certificate until you replace it, so verification is
# skipped here. Point smtp_server at the certificate's JMAIL_ORIGIN and drop the
# next two lines once you use a certificate from a public CA.
context = ssl.create_default_context()
context.check_hostname = False
context.verify_mode = ssl.CERT_NONE

# Try to log in to server and send email
try:
    msg = MIMEText(email_body)
    msg["Subject"] = "Test email from Jmail!"
    msg["From"] = sender
    msg["To"] = receiver

    with smtplib.SMTP(smtp_server, smtp_port) as server:
        server.starttls(context=context)
        server.login(smtp_auth_user, smtp_auth_password)
        server.sendmail(sender, [receiver], msg.as_string())
        print("Email sent")
except (smtplib.SMTPException, OSError) as exc:
    # Print any error messages to stdout
    print(exc)
